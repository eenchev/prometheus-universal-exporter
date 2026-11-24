package repository

import (
	"strings"
	"testing"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
)

// Every type of secret store the source registers is documented: the user
// page's table of an entry names it as a value of type, the specification's
// section has a rule that begins with it, and the schema takes it.
func TestEverySecretStoreTypeIsDocumented(t *testing.T) {
	page := read(t, "docs/AUTHENTICATION.md")
	_, stores, found := strings.Cut(page, "\n## Secret stores\n")
	if !found {
		t.Fatal("docs/AUTHENTICATION.md has no \"## Secret stores\" section")
	}
	_, spec, found := strings.Cut(read(t, "docs/SPECIFICATION-EXPORTER.md"), "\n## 42.6c Secret stores\n")
	if !found {
		t.Fatal("the specification has no § 42.6c")
	}
	spec, _, _ = strings.Cut(spec, "\n## ")
	schema := read(t, "configs/config.schema.json")
	types := fetch.SecretStoreTypeNames()
	if len(types) < 3 {
		t.Fatalf("the source registers the secret store types %v; the registry has moved", types)
	}
	for _, name := range types {
		if !strings.Contains(stores, "`type: "+name+"`") && !strings.Contains(stores, "    type: "+name+"\n") {
			t.Errorf("docs/AUTHENTICATION.md has no example of a secret store of type %s", name)
		}
		if !strings.Contains(spec, "\n- `"+name+"` MUST ") {
			t.Errorf("§ 42.6c has no rule that begins \"- `%s` MUST\"", name)
		}
		if !strings.Contains(schema, `"`+name+`"`) {
			t.Errorf("configs/config.schema.json does not take the type %s", name)
		}
	}
}
