// broker/internal/toolproxy/office_pptx.go
//
// pptx.create/edit/extract/thumbnail — office-worker-backed PowerPoint tools.
// create/edit/extract share the docx engines (script-create, zip_edit,
// markdown/xml extract); thumbnail is pptx's own multi-output op (one image
// file part per rendered page, API.md), so it gets a dedicated handler that
// writes each returned page under output_dir.
package toolproxy

import (
	"context"
	"fmt"
	"path/filepath"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

type pptxCreatePlugin struct{}

func (pptxCreatePlugin) ToolID() string            { return "pptx.create" }
func (pptxCreatePlugin) Available(cfg Config) bool { return officeAvailable(cfg) }
func (pptxCreatePlugin) Build(cfg Config) Handler {
	return newOfficeScriptCreateHandler(cfg, "pptx.create", "/v1/pptx/create")
}
func (pptxCreatePlugin) Scope() string                   { return "pptx:write" }
func (pptxCreatePlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_WRITE_LOCAL }

type pptxEditPlugin struct{}

func (pptxEditPlugin) ToolID() string            { return "pptx.edit" }
func (pptxEditPlugin) Available(cfg Config) bool { return officeAvailable(cfg) }
func (pptxEditPlugin) Build(cfg Config) Handler {
	return newOfficeXMLEditHandler(cfg, "pptx.edit", "/v1/pptx/edit")
}
func (pptxEditPlugin) Scope() string                   { return "pptx:write" }
func (pptxEditPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_WRITE_LOCAL }

type pptxExtractPlugin struct{}

func (pptxExtractPlugin) ToolID() string            { return "pptx.extract" }
func (pptxExtractPlugin) Available(cfg Config) bool { return officeAvailable(cfg) }
func (pptxExtractPlugin) Build(cfg Config) Handler {
	return newOfficeExtractHandler(cfg, "pptx.extract", "/v1/pptx/extract")
}
func (pptxExtractPlugin) Scope() string                   { return "pptx:read" }
func (pptxExtractPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_READ_ONLY }

type pptxThumbnailPlugin struct{}

func (pptxThumbnailPlugin) ToolID() string                  { return "pptx.thumbnail" }
func (pptxThumbnailPlugin) Available(cfg Config) bool       { return officeAvailable(cfg) }
func (pptxThumbnailPlugin) Build(cfg Config) Handler        { return newPPTXThumbnailHandler(cfg) }
func (pptxThumbnailPlugin) Scope() string                   { return "pptx:write" }
func (pptxThumbnailPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_WRITE_LOCAL }

func init() {
	RegisterPlugin(pptxCreatePlugin{})
	RegisterPlugin(pptxEditPlugin{})
	RegisterPlugin(pptxExtractPlugin{})
	RegisterPlugin(pptxThumbnailPlugin{})
}

// newPPTXThumbnailHandler renders slides to page images (soffice→pdf→pdftoppm,
// API.md), writing each returned image under output_dir/<name>. The worker's
// own 20-page default cap + dpi param ride straight through as request params.
func newPPTXThumbnailHandler(cfg Config) Handler {
	client := newOfficeClient(cfg)
	ws := cfg.withSeam()
	const toolID = "pptx.thumbnail"
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		path, _ := req.Args["path"].(string)
		outputDir, _ := req.Args["output_dir"].(string)
		if outputDir == "" {
			return nil, 0, fmt.Errorf("%s: missing required arg %q", toolID, "output_dir")
		}
		data, err := officeReadInput(ctx, ws, req, path, toolID)
		if err != nil {
			return nil, 0, err
		}
		params := map[string]any{}
		for _, key := range []string{"dpi", "max_pages"} {
			if v, ok := req.Args[key]; ok {
				params[key] = v
			}
		}
		files := []officeFile{{Field: "file", Filename: filepath.Base(path), Data: data}}
		result, err := client.call(ctx, "/v1/pptx/thumbnail", files, params)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", toolID, err)
		}

		paths := make([]any, 0, len(result.Files))
		var totalBytes int64
		for i, f := range result.Files {
			name := f.Filename
			if name == "" {
				name = fmt.Sprintf("page-%d.jpg", i+1)
			}
			rel := filepath.Join(outputDir, name)
			if err := officeWriteOutput(ctx, ws, req, rel, toolID, f.Data); err != nil {
				return nil, 0, err
			}
			paths = append(paths, rel)
			totalBytes += int64(len(f.Data))
		}
		out := map[string]any{"paths": paths}
		for k, v := range result.Result {
			out[k] = v
		}
		return out, totalBytes/1024 + 1, nil
	}
}
