//go:build !select_request_types || request_type_sshfile

package exporter

import "testing"

// The debug page asks a run of an sshfile collector for the host, its port
// 22 unless written, the account being request.username; a pasted response
// is the file's content.
func TestTheDebugPageAsksAnSSHFileCollectorForTheHost(t *testing.T) {
	h := newSSHHost(t)
	server := debugFormServer(t, `  - name: remote
    request:
      type: sshfile
      root: /srv
      path: app.prom
      username: metrics
      private_key_file: `+h.keyFile+`
      known_hosts_file: `+h.dir+`/known_hosts
    transform: {type: prometheus}
`)
	page, form := debugFormPage(t, server, "remote")
	want := debugForm{RequestType: "sshfile", Answer: "Paste the file's content", ContentType: true, TargetLabel: "SSH host", TargetHint: "host or host:22", Params: "also: path"}
	if form != want {
		t.Errorf("the form is %+v, want %+v", form, want)
	}
	requireDebugForm(t, page, form)
}
