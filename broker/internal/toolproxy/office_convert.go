// broker/internal/toolproxy/office_convert.go
//
// office.convert — any↔any LibreOffice conversion, including legacy .doc→.docx
// (API.md: the input filter is selected from the uploaded part's own filename
// extension, or source_extension as an override).
package toolproxy

import (
	"context"
	"fmt"
	"path/filepath"

	planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"
)

type officeConvertPlugin struct{}

func (officeConvertPlugin) ToolID() string                  { return "office.convert" }
func (officeConvertPlugin) Available(cfg Config) bool       { return officeAvailable(cfg) }
func (officeConvertPlugin) Build(cfg Config) Handler        { return newOfficeConvertHandler(cfg) }
func (officeConvertPlugin) Scope() string                   { return "office:write" }
func (officeConvertPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_WRITE_LOCAL }

func init() { RegisterPlugin(officeConvertPlugin{}) }

func newOfficeConvertHandler(cfg Config) Handler {
	client := newOfficeClient(cfg)
	ws := cfg.withSeam()
	const toolID = "office.convert"
	return func(ctx context.Context, req Request) (map[string]any, int64, error) {
		path, _ := req.Args["path"].(string)
		outputPath, _ := req.Args["output_path"].(string)
		targetFormat, _ := req.Args["target_format"].(string)
		if targetFormat == "" {
			return nil, 0, fmt.Errorf("%s: missing required arg %q", toolID, "target_format")
		}
		if outputPath == "" {
			return nil, 0, fmt.Errorf("%s: missing required arg %q", toolID, "output_path")
		}
		data, err := officeReadInput(ctx, ws, req, path, toolID)
		if err != nil {
			return nil, 0, err
		}
		params := map[string]any{"target_format": targetFormat}
		if v, ok := req.Args["source_extension"]; ok {
			params["source_extension"] = v
		}
		files := []officeFile{{Field: "file", Filename: filepath.Base(path), Data: data}}
		result, err := client.call(ctx, "/v1/office/convert", files, params)
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
