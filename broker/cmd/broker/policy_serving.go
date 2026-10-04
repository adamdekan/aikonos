package main

// policy_serving.go — the broker as OPA's policy source, and the OpenFGA
// authorization model it pins. Together they let every decision record name
// the exact policy behind it (docs/15-decision-replay.md).

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/spf13/viper"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/adamdekan/aikonos/broker/internal/audit"
	"github.com/adamdekan/aikonos/broker/internal/ids"
	"github.com/adamdekan/aikonos/broker/internal/policy"
	auditv1 "github.com/adamdekan/aikonos/gen/go/audit/v1"
)

// policyBundleName is the OPA bundle (and its only root package) the broker
// serves; deploy/compose/opa.yaml points OPA's bundle plugin at it.
const policyBundleName = "aikonos"

// startPolicyBundle serves the Rego policy under policy.bundle_dir to OPA as
// the "aikonos" bundle. Each revision is archived in the audit store before
// OPA can load it and recorded as an aikonos.broker.policy.loaded event. It
// returns the bundle name for policy.Config.PolicyBundle, or "" when
// policy.bundle_dir is unset: OPA then loads its own policy, and decisions are
// recorded without a revision.
func startPolicyBundle(ctx context.Context, emitter *audit.Emitter, tenant string, log *zap.Logger) string {
	dir := viper.GetString("policy.bundle_dir")
	if dir == "" {
		log.Warn("policy.bundle_dir is unset: OPA loads its own policy, so decision records carry no policy revision and cannot be replayed")
		return ""
	}
	var archive policy.BundleArchive
	if emitter.Persistent() {
		archive = emitter
	} else {
		log.Warn("policy bundle: no audit store configured, so policy revisions are served without being archived")
	}
	srv := policy.NewBundleServer(dir, []string{policyBundleName}, archive, log)
	srv.OnChange = func(prev, next *policy.Bundle) {
		emitPolicyEvent(ctx, emitter, tenant, "aikonos.broker.policy.loaded", "aikonos:policy:"+next.Revision, policyLoadedContext(prev, next, archive != nil), log)
	}
	if _, err := srv.Reload(ctx); err != nil {
		log.Fatal("policy bundle: initial load failed", zap.String("dir", dir), zap.Error(err))
	}
	go srv.Run(ctx, time.Duration(viper.GetInt("policy.bundle_reload_seconds"))*time.Second)

	mux := http.NewServeMux()
	mux.Handle("/opa/bundles/"+policyBundleName+".tar.gz", srv)
	httpSrv := &http.Server{
		Addr:              viper.GetString("policy.bundle_http_addr"),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		// Without this listener OPA never loads a policy and every decision
		// fails closed, so a bind failure is fatal rather than silent.
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("policy bundle: HTTP listener failed", zap.String("addr", httpSrv.Addr), zap.Error(err))
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(shutdownCtx)
	}()
	log.Info("policy bundle: serving OPA", zap.String("addr", httpSrv.Addr), zap.String("dir", dir))
	return policyBundleName
}

func policyLoadedContext(prev, next *policy.Bundle, archived bool) map[string]any {
	files := make([]any, 0, len(next.Files))
	for _, f := range next.Files {
		files = append(files, map[string]any{"path": f.Path, "sha256": f.SHA256})
	}
	ctx := map[string]any{"revision": next.Revision, "files": files, "archived": archived}
	if prev != nil {
		ctx["previous_revision"] = prev.Revision
	}
	return ctx
}

// startFGAModelRefresh pins OpenFGA checks to one authorization model and
// records it. With policy.openfga_model_id set that model is used throughout;
// otherwise the store's latest is resolved now and re-resolved every
// policy.fga_model_refresh_seconds. Each model checks start using is recorded
// as an aikonos.broker.policy.fga_model event.
func startFGAModelRefresh(ctx context.Context, engine *policy.Engine, emitter *audit.Emitter, tenant string, log *zap.Logger) {
	if !engine.FGAEnabled() {
		return
	}
	source := "latest"
	if viper.GetString("policy.openfga_model_id") != "" {
		source = "configured"
	}
	refresh := func(initial bool) {
		prev, next, err := engine.RefreshFGAModel(ctx)
		if err != nil {
			log.Warn("OpenFGA model refresh failed; checks keep their current model", zap.String("model_id", prev), zap.Error(err))
			return
		}
		if next == "" || (!initial && next == prev) {
			return
		}
		log.Info("OpenFGA checks pinned to authorization model", zap.String("model_id", next), zap.String("source", source))
		details := map[string]any{"model_id": next, "source": source}
		if prev != "" && prev != next {
			details["previous_model_id"] = prev
		}
		emitPolicyEvent(ctx, emitter, tenant, "aikonos.broker.policy.fga_model", "aikonos:fga_model:"+next, details, log)
	}
	refresh(true)
	if source == "configured" {
		return
	}
	interval := time.Duration(viper.GetInt("policy.fga_model_refresh_seconds")) * time.Second
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				refresh(false)
			}
		}
	}()
}

// emitPolicyEvent records a change to the policy that decides (fire-and-forget).
func emitPolicyEvent(ctx context.Context, emitter *audit.Emitter, tenant, eventType, resource string, details map[string]any, log *zap.Logger) {
	ctxStruct, err := structpb.NewStruct(details)
	if err != nil {
		log.Error("policy event context could not be encoded", zap.String("event_type", eventType), zap.Error(err))
	}
	if err := emitter.Emit(ctx, &auditv1.AuditEvent{
		EventId:     ids.EventID(),
		TenantId:    tenant,
		OccurredAt:  timestamppb.New(time.Now().UTC()),
		EventType:   eventType,
		ResourceRef: resource,
		Decision:    auditv1.PolicyDecision_ALLOW,
		Context:     ctxStruct,
	}); err != nil {
		audit.RecordEmitFailure(ctx, log, err, eventType)
	}
}
