//go:build !select_request_types || request_type_redis || request_type_s3file || request_type_tcp

package fetch

// optInRequestKeys are, by opt-in type, the request keys that type accepts
// and no type of a default build does. A default build, which leaves the
// opt-in types out, cannot see them claimed (TestEveryRequestKeyBelongsToAType),
// and a build with the type keeps the list in step with what the type
// accepts (checkOptInRequestKeys).
var optInRequestKeys = map[string][]string{
	RequestTypeS3File: {"region", "endpoint", "path_style", "access_key_id_file", "secret_access_key_file", "session_token_file"},
	RequestTypeRedis:  {"db", "info_sections", "commands"},
	RequestTypeTCP:    {"send", "terminator"},
}
