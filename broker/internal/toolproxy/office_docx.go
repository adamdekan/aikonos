// broker/internal/toolproxy/office_docx.go
//
// docx.create/edit/extract — office-worker-backed Word document tools.
// docx.create shares the model-authored-script engine with xlsx/pptx/pdf's
// *.create (newOfficeScriptCreateHandler); docx.edit shares the declarative
// zip_edit engine with pptx.edit (newOfficeXMLEditHandler); docx.extract
// shares the markdown/format:"xml" modes with pptx.extract
// (newOfficeExtractHandler). See office_client.go.
package toolproxy

import planv1 "github.com/adamdekan/aikonos/gen/go/plan/v1"

type docxCreatePlugin struct{}

func (docxCreatePlugin) ToolID() string            { return "docx.create" }
func (docxCreatePlugin) Available(cfg Config) bool { return officeAvailable(cfg) }
func (docxCreatePlugin) Build(cfg Config) Handler {
	return newOfficeScriptCreateHandler(cfg, "docx.create", "/v1/docx/create")
}
func (docxCreatePlugin) Scope() string                   { return "docx:write" }
func (docxCreatePlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_WRITE_LOCAL }

type docxEditPlugin struct{}

func (docxEditPlugin) ToolID() string            { return "docx.edit" }
func (docxEditPlugin) Available(cfg Config) bool { return officeAvailable(cfg) }
func (docxEditPlugin) Build(cfg Config) Handler {
	return newOfficeXMLEditHandler(cfg, "docx.edit", "/v1/docx/edit")
}
func (docxEditPlugin) Scope() string                   { return "docx:write" }
func (docxEditPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_WRITE_LOCAL }

type docxExtractPlugin struct{}

func (docxExtractPlugin) ToolID() string            { return "docx.extract" }
func (docxExtractPlugin) Available(cfg Config) bool { return officeAvailable(cfg) }
func (docxExtractPlugin) Build(cfg Config) Handler {
	return newOfficeExtractHandler(cfg, "docx.extract", "/v1/docx/extract")
}
func (docxExtractPlugin) Scope() string                   { return "docx:read" }
func (docxExtractPlugin) EffectClass() planv1.EffectClass { return planv1.EffectClass_READ_ONLY }

func init() {
	RegisterPlugin(docxCreatePlugin{})
	RegisterPlugin(docxEditPlugin{})
	RegisterPlugin(docxExtractPlugin{})
}
