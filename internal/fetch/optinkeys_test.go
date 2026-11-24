//go:build !select_request_types || request_type_ftp || request_type_imap || request_type_modbus || request_type_mqtt || request_type_pop3 || request_type_radius || request_type_sip || request_type_smtp || request_type_websocket || request_type_whois

package fetch

// optInRequestKeys are, by opt-in type, the request keys of
// model.RequestConfig's common part that type accepts and no type of a
// default build does. A default build, which leaves the opt-in types out,
// cannot see them claimed (TestEveryRequestKeyBelongsToAType), and a build
// with the type keeps the list in step with what the type accepts
// (checkOptInRequestKeys). The file's constraint names the default build
// and every opt-in type. A key is listed only when two opt-in types take
// it, which makes it the common part's: the keys of one type alone are its
// own (RequestType.Config), which every build knows from the catalogue.
// auth and xoauth2 are how smtp, imap and pop3 log in, and allow_plaintext
// lets imap and pop3 do it without TLS.
var optInRequestKeys = map[string][]string{
	RequestTypeSMTP: {"auth", "xoauth2"},
	RequestTypeIMAP: {"auth", "xoauth2", "allow_plaintext"},
	RequestTypePOP3: {"auth", "xoauth2", "allow_plaintext"},
}

// optInTargetKeys are, by opt-in type, the keys of a static target's request
// that type accepts and no type of a default build does, as optInRequestKeys
// are of a collector's, an opt-in type's target keys that are the common
// part's.
var optInTargetKeys = map[string][]string{}
