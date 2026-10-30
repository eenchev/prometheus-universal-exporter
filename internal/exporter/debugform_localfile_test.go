//go:build !select_request_types || request_type_localfile

package exporter

import "testing"

// The debug page asks a run of a localfile collector for what its target is
// under its root, and says it may be left out: the directory that holds the
// file request.path names, the directory of the files it reads, or else the
// file; a pasted response is the file's content, whose Content-Type may
// choose its decoder, with no status or headers, which a file has none of.
func TestTheDebugPageAsksALocalfileCollectorForAPathUnderItsRoot(t *testing.T) {
	root := t.TempDir()
	server := debugFormServer(t, `  - name: named
    request: {type: localfile, root: `+root+`, path: app.prom}
    transform: {type: prometheus}
  - name: textfiles
    request: {type: localfile, root: `+root+`, files: ["*.prom"]}
    transform: {type: prometheus}
  - name: any
    request: {type: localfile, root: `+root+`}
    transform: {type: prometheus}
`)
	for name, want := range map[string]debugForm{
		"named":     {TargetLabel: "Directory", TargetHint: "a directory under its root that holds app.prom (optional)", Params: "also: path"},
		"textfiles": {TargetLabel: "Directory", TargetHint: "a directory under its root (optional)"},
		"any":       {TargetLabel: "File", TargetHint: "a file under its root, such as app.prom (optional)", Params: "also: path"},
	} {
		want.RequestType, want.Answer, want.ContentType = "localfile", "Paste the file's content", true
		page, form := debugFormPage(t, server, name)
		if form != want {
			t.Errorf("%s: the form is %+v, want %+v", name, form, want)
		}
		requireDebugForm(t, page, form)
	}
}
