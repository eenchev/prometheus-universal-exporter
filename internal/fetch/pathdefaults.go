//go:build !select_request_types || request_type_http || request_type_graphite || request_type_localfile

package fetch

import "fmt"

// The load-time rule of a request.path placeholder's default, for the types
// that bind path parameters: http and graphite, in a URL's path, and
// localfile, in a file's. grpc has no request.path, so a build with only it
// leaves this out.

// checkPathParamDefaults refuses, when the configuration loads, a
// request.path placeholder whose default no probe could be sent with: every
// probe that left the parameter out would fail, with an error that blames
// the probe. check is the function the request type holds a value to when a
// probe binds it, so a default is refused at load by exactly the rule that
// would refuse it at every probe.
func checkPathParamDefaults(collector string, placeholders []pathPlaceholder, check func(name, value string) error) error {
	for _, p := range placeholders {
		if !p.HasDefault {
			continue
		}
		if err := check(p.Name, p.Default); err != nil {
			return fmt.Errorf("collector %q request.path: the default of %s is %q, which no probe could use (%w), so every probe that leaves %s out would fail; change the default", collector, p.Name, p.Default, err, p.Name)
		}
	}
	return nil
}
