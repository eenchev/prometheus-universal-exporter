//go:build !select_request_types || request_type_ldap || request_type_redis || request_type_s3file || request_type_tcp

package fetch

// optInRequestKeys are, by opt-in type, the request keys of
// model.RequestConfig's common part that type accepts and no type of a
// default build does. A default build, which leaves the opt-in types out,
// cannot see them claimed (TestEveryRequestKeyBelongsToAType), and a build
// with the type keeps the list in step with what the type accepts
// (checkOptInRequestKeys). There are none: a key only opt-in types take
// would have to be taken by two of them to be the common part's, and the
// keys of one type alone are its own (RequestType.Config), which every
// build knows from the catalogue.
var optInRequestKeys = map[string][]string{}

// optInTargetKeys are, by opt-in type, the keys of a static target's request
// that type accepts and no type of a default build does, as optInRequestKeys
// are of a collector's.
var optInTargetKeys = map[string][]string{
	RequestTypeLDAP: {"bind_dn"},
}
