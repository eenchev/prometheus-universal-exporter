//go:build !select_request_types || request_type_ftp || request_type_imap || request_type_modbus || request_type_mqtt || request_type_smtp || request_type_websocket

package fetch

// optInRequestKeys are, by opt-in type, the request keys of
// model.RequestConfig's common part that type accepts and no type of a
// default build does. A default build, which leaves the opt-in types out,
// cannot see them claimed (TestEveryRequestKeyBelongsToAType), and a build
// with the type keeps the list in step with what the type accepts
// (checkOptInRequestKeys). The file's constraint names the default build
// and every opt-in type. A type's list is empty all the same: a key only
// opt-in types take
// would have to be taken by two of them to be the common part's, and the
// keys of one type alone are its own (RequestType.Config), which every
// build knows from the catalogue.
var optInRequestKeys = map[string][]string{}

// optInTargetKeys are, by opt-in type, the keys of a static target's request
// that type accepts and no type of a default build does, as optInRequestKeys
// are of a collector's, an opt-in type's target keys that are the common
// part's.
var optInTargetKeys = map[string][]string{}
