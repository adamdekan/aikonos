// broker/internal/toolproxy/office_pdf.go
//
// pdf.create/transform/extract — office-worker-backed PDF tools. pdf.create
// shares the model-authored-script engine (reportlab); pdf.transform is
// fully declarative (qpdf + pypdf, API.md: merge/split/rotate/decrypt/
// fill-forms) with no script and no shared engine, since each op has its own
// input/output cardinality; pdf.extract is a declarative text/table pull with
// an OCR fallback, no output file.
package toolproxy

import (
	"context"
	"fmt"
	"path/filepath"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

type pdfCreatePlugin struct{}

func (pdfCreatePlugin) ToolID() string            { return "pdf.create" }
func (pdfCreatePlugin) Available(cfg Config) bool { return officeAvailable(cfg) }
func (pdfCreatePlugin) Build(cfg Config) Handler {
	return newOfficeScriptCreateHandler(cfg, "pdf.create", "/v1/pdf/create")
}
func (pdfCreatePlugin) Scope() string                   { return "pdf:write" }
func (pdfCreatePlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_WRITE_LOCAL }

type pdfTransformPlugin struct{}

func (pdfTransformPlugin) ToolID() string                  { return "pdf.transform" }
func (pdfTransformPlugin) Available(cfg Config) bool       { return officeAvailable(cfg) }
func (pdfTransformPlugin) Build(cfg Config) Handler        { return newPDFTransformHandler(cfg) }
func (pdfTransformPlugin) Scope() string                   { return "pdf:write" }
func (pdfTransformPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_WRITE_LOCAL }

type pdfExtractPlugin struct{}

func (pdfExtractPlugin) ToolID() string                  { return "pdf.extract" }
func (pdfExtractPlugin) Available(cfg Config) bool       { return officeAvailable(cfg) }
func (pdfExtractPlugin) Build(cfg Config) Handler        { return newPDFExtractHandler(cfg) }
func (pdfExtractPlugin) Scope() string                   { return "pdf:read" }
func (pdfExtractPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_READ_ONLY }

func init() {
	RegisterPlugin(pdfCreatePlugin{})
	RegisterPlugin(pdfTransformPlugin{})
	RegisterPlugin(pdfExtractPlugin{})
}

func newPDFExtractHandler(cfg Config) Handler {
	client := newOfficeClient(cfg)
	ws := cfg.withSeam()
	const toolID = "pdf.extract"
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		path, _ := req.Args["path"].(string)
		data, err := officeReadInput(ctx, ws, req, path, toolID)
		if err != nil {
			return nil, 0, err
		}
		params := map[string]any{}
		if v, ok := req.Args["ocr"]; ok {
			params["ocr"] = v
		}
		files := []officeFile{{Field: "file", Filename: filepath.Base(path), Data: data}}
		result, err := client.call(ctx, "/v1/pdf/extract", files, params)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", toolID, err)
		}
		out := map[string]any{"path": path}
		for k, v := range result.Result {
			out[k] = v
		}
		text, _ := out["text"].(string)
		// Injection-pattern scan (result_scan.go, checkpoint 10): the extracted
		// "text" field is untrusted external text; a match annotates the
		// result but never blocks/alters it.
		annotateInjectionFlags(out, text)
		return out, int64(len(text)/1024) + 1, nil
	}
}

// newPDFTransformHandler dispatches the five declarative pdf.transform ops.
// Every op reads its input file(s) from req.Args["paths"] (workspace-relative,
// order preserved — significant for "merge"); merge/rotate/decrypt/fill-forms
// write a single output_path, split writes one output per req.Args["outputs"]
// entry (mirroring the worker's per-range file-part order, API.md).
func newPDFTransformHandler(cfg Config) Handler {
	client := newOfficeClient(cfg)
	ws := cfg.withSeam()
	const toolID = "pdf.transform"
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		op, _ := req.Args["op"].(string)
		if op == "" {
			return nil, 0, fmt.Errorf("%s: missing required arg %q", toolID, "op")
		}
		paths, _ := req.Args["paths"].([]any)
		if len(paths) == 0 {
			return nil, 0, fmt.Errorf("%s: missing required arg %q", toolID, "paths")
		}
		files := make([]officeFile, 0, len(paths))
		for _, p := range paths {
			rel, _ := p.(string)
			data, err := officeReadInput(ctx, ws, req, rel, toolID)
			if err != nil {
				return nil, 0, err
			}
			files = append(files, officeFile{Field: "file", Filename: filepath.Base(rel), Data: data})
		}

		var outputs []any
		params := map[string]any{"op": op}
		switch op {
		case "merge":
			// no op-specific params
		case "rotate":
			if v, ok := req.Args["angle"]; ok {
				params["angle"] = v
			}
			if v, ok := req.Args["pages"]; ok {
				params["pages"] = v
			}
		case "decrypt":
			if v, ok := req.Args["password"]; ok {
				params["password"] = v
			}
		case "fill-forms":
			if v, ok := req.Args["fields"]; ok {
				params["fields"] = v
			}
		case "split":
			outputs, _ = req.Args["outputs"].([]any)
			ranges := make([]any, 0, len(outputs))
			for _, o := range outputs {
				m, _ := o.(map[string]any)
				ranges = append(ranges, map[string]any{"pages": m["pages"]})
			}
			params["ranges"] = ranges
		default:
			return nil, 0, fmt.Errorf("%s: unknown op %q", toolID, op)
		}

		result, err := client.call(ctx, "/v1/pdf/transform", files, params)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", toolID, err)
		}
		if len(result.Files) == 0 {
			return nil, 0, fmt.Errorf("%s: office-worker returned no output file", toolID)
		}

		if op == "split" {
			if len(outputs) != len(result.Files) {
				return nil, 0, fmt.Errorf("%s: office-worker returned %d files for %d requested ranges", toolID, len(result.Files), len(outputs))
			}
			outPaths := make([]any, 0, len(outputs))
			var totalBytes int64
			for i, o := range outputs {
				m, _ := o.(map[string]any)
				outputPath, _ := m["output_path"].(string)
				if outputPath == "" {
					return nil, 0, fmt.Errorf("%s: outputs[%d] missing required field %q", toolID, i, "output_path")
				}
				if err := officeWriteOutput(ctx, ws, req, outputPath, toolID, result.Files[i].Data); err != nil {
					return nil, 0, err
				}
				outPaths = append(outPaths, outputPath)
				totalBytes += int64(len(result.Files[i].Data))
			}
			out := map[string]any{"paths": outPaths}
			for k, v := range result.Result {
				out[k] = v
			}
			return out, totalBytes/1024 + 1, nil
		}

		outputPath, _ := req.Args["output_path"].(string)
		if outputPath == "" {
			return nil, 0, fmt.Errorf("%s: missing required arg %q", toolID, "output_path")
		}
		data := result.Files[0].Data
		if err := officeWriteOutput(ctx, ws, req, outputPath, toolID, data); err != nil {
			return nil, 0, err
		}
		return map[string]any{"path": outputPath, "size": len(data)}, int64(len(data)/1024) + 1, nil
	}
}
