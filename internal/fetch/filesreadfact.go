//go:build !select_request_types || request_type_s3file || request_type_sshfile

package fetch

// The spans of the request types that read remote files, sshfile's and
// s3file's, tell how many files a read took and how many bytes they were;
// localfile's reads are no request and have no span.

// filesRead is how many of a directory's files were read: those with a
// response, not those that failed.
func filesRead(read *DirectoryRead) int {
	n := 0
	for i := range read.Files {
		if read.Files[i].Response != nil {
			n++
		}
	}
	return n
}
