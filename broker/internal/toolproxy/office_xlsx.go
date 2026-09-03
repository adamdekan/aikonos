// broker/internal/toolproxy/office_xlsx.go
//
// xlsx.create/edit/extract/recalc — office-worker-backed Excel tools.
// Unlike docx/pptx.edit, xlsx.edit runs a model-authored openpyxl script
// against the existing workbook (API.md: "xlsx.edit request: file part +
// params: {script, output_filename?}") rather than declarative zip ops, so it
// gets its own handler instead of newOfficeXMLEditHandler.
package toolproxy

import (
	"context"
	"fmt"
	"path/filepath"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

type xlsxCreatePlugin struct{}

func (xlsxCreatePlugin) ToolID() string            { return "xlsx.create" }
func (xlsxCreatePlugin) Available(cfg Config) bool { return officeAvailable(cfg) }
func (xlsxCreatePlugin) Build(cfg Config) Handler {
	return newOfficeScriptCreateHandler(cfg, "xlsx.create", "/v1/xlsx/create")
}
func (xlsxCreatePlugin) Scope() string                   { return "xlsx:write" }
func (xlsxCreatePlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_WRITE_LOCAL }

type xlsxEditPlugin struct{}

func (xlsxEditPlugin) ToolID() string                  { return "xlsx.edit" }
func (xlsxEditPlugin) Available(cfg Config) bool       { return officeAvailable(cfg) }
func (xlsxEditPlugin) Build(cfg Config) Handler        { return newXLSXEditHandler(cfg) }
func (xlsxEditPlugin) Scope() string                   { return "xlsx:write" }
func (xlsxEditPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_WRITE_LOCAL }

type xlsxExtractPlugin struct{}

func (xlsxExtractPlugin) ToolID() string                  { return "xlsx.extract" }
func (xlsxExtractPlugin) Available(cfg Config) bool       { return officeAvailable(cfg) }
func (xlsxExtractPlugin) Build(cfg Config) Handler        { return newXLSXExtractHandler(cfg) }
func (xlsxExtractPlugin) Scope() string                   { return "xlsx:read" }
func (xlsxExtractPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_READ_ONLY }

type xlsxRecalcPlugin struct{}

func (xlsxRecalcPlugin) ToolID() string                  { return "xlsx.recalc" }
func (xlsxRecalcPlugin) Available(cfg Config) bool       { return officeAvailable(cfg) }
func (xlsxRecalcPlugin) Build(cfg Config) Handler        { return newXLSXRecalcHandler(cfg) }
func (xlsxRecalcPlugin) Scope() string                   { return "xlsx:write" }
func (xlsxRecalcPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_WRITE_LOCAL }

func init() {
	RegisterPlugin(xlsxCreatePlugin{})
	RegisterPlugin(xlsxEditPlugin{})
	RegisterPlugin(xlsxExtractPlugin{})
	RegisterPlugin(xlsxRecalcPlugin{})
}

func newXLSXEditHandler(cfg Config) Handler {
	client := newOfficeClient(cfg)
	ws := cfg.withSeam()
	const toolID = "xlsx.edit"
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		path, _ := req.Args["path"].(string)
		outputPath, _ := req.Args["output_path"].(string)
		script := argString(req.Args, "script")
		if script == "" {
			return nil, 0, fmt.Errorf("%s: missing required arg %q", toolID, "script")
		}
		if outputPath == "" {
			return nil, 0, fmt.Errorf("%s: missing required arg %q", toolID, "output_path")
		}
		data, err := officeReadInput(ctx, ws, req, path, toolID)
		if err != nil {
			return nil, 0, err
		}
		files := []officeFile{{Field: "file", Filename: filepath.Base(path), Data: data}}
		result, err := client.call(ctx, "/v1/xlsx/edit", files, map[string]any{"script": script})
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", toolID, err)
		}
		if len(result.Files) == 0 {
			return nil, 0, fmt.Errorf("%s: office-worker returned no output file", toolID)
		}
		out := result.Files[0].Data
		if err := officeWriteOutput(ctx, ws, req, outputPath, toolID, out); err != nil {
			return nil, 0, err
		}
		return map[string]any{"path": outputPath, "size": len(out)}, int64(len(out)/1024) + 1, nil
	}
}

func newXLSXExtractHandler(cfg Config) Handler {
	client := newOfficeClient(cfg)
	ws := cfg.withSeam()
	const toolID = "xlsx.extract"
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		path, _ := req.Args["path"].(string)
		data, err := officeReadInput(ctx, ws, req, path, toolID)
		if err != nil {
			return nil, 0, err
		}
		params := map[string]any{}
		for _, key := range []string{"sheet", "range", "format"} {
			if v, ok := req.Args[key]; ok {
				params[key] = v
			}
		}
		files := []officeFile{{Field: "file", Filename: filepath.Base(path), Data: data}}
		result, err := client.call(ctx, "/v1/xlsx/extract", files, params)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", toolID, err)
		}
		out := map[string]any{"path": path}
		for k, v := range result.Result {
			out[k] = v
		}
		// Injection-pattern scan (result_scan.go, checkpoint 10): the extracted
		// "content" field is untrusted external text; a match annotates the
		// result but never blocks/alters it.
		content, _ := out["content"].(string)
		annotateInjectionFlags(out, content)
		return out, 1, nil
	}
}

func newXLSXRecalcHandler(cfg Config) Handler {
	client := newOfficeClient(cfg)
	ws := cfg.withSeam()
	const toolID = "xlsx.recalc"
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		path, _ := req.Args["path"].(string)
		outputPath, _ := req.Args["output_path"].(string)
		if outputPath == "" {
			return nil, 0, fmt.Errorf("%s: missing required arg %q", toolID, "output_path")
		}
		data, err := officeReadInput(ctx, ws, req, path, toolID)
		if err != nil {
			return nil, 0, err
		}
		files := []officeFile{{Field: "file", Filename: filepath.Base(path), Data: data}}
		result, err := client.call(ctx, "/v1/xlsx/recalc", files, map[string]any{})
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", toolID, err)
		}
		if len(result.Files) == 0 {
			return nil, 0, fmt.Errorf("%s: office-worker returned no output file", toolID)
		}
		out := result.Files[0].Data
		if err := officeWriteOutput(ctx, ws, req, outputPath, toolID, out); err != nil {
			return nil, 0, err
		}
		outMap := map[string]any{"path": outputPath, "size": len(out)}
		for _, k := range []string{"errors", "error_count"} {
			if v, ok := result.Result[k]; ok {
				outMap[k] = v
			}
		}
		return outMap, int64(len(out)/1024) + 1, nil
	}
}
