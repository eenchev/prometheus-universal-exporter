//go:build !select_request_types || request_type_dns || request_type_grpc || request_type_icmp || request_type_ldap || request_type_ntp || request_type_redis || request_type_s3file || request_type_sql || request_type_sshexec || request_type_sshfile || request_type_tcp || request_type_tlscert

package fetch

// intFact makes a TraceFact of a number, for the request types that tell
// numbers of their requests (TracedRequest.Facts).
func intFact(key string, value int) TraceFact { return TraceFact{Key: key, Int: int64(value)} }
