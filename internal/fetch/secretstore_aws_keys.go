package fetch

// The keys of an aws secret store, which every build knows, the ones
// without the store too, so that the schema is the same for all of them and
// a build without it refuses an entry for its type and not for its keys
// (secretstore_aws.go, secretstore_aws_none.go).
var awsStoreKeys = []string{"region", "endpoint"}

func awsStoreKeyRules() map[string]map[string]any {
	return map[string]map[string]any{
		"region":   {"description": "aws: the region of the secrets; the AWS SDK's own configuration, such as AWS_REGION, says it when left out."},
		"endpoint": {"description": "aws: the URL of the API in place of the public one, such as a VPC endpoint."},
	}
}
