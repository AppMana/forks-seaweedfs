package command

// An explicit S3 disk scope must not be silently replaced with the filer's
// larger scope. Both scopes still use the same backing directory and whole
// disk limit; their reader allocations share the parent's process budget.
func configureEmbeddedS3Scope(options *S3Options, directory string, wholeMB int64) bool {
	if options.cacheSizeMB == nil || *options.cacheSizeMB <= 0 {
		return false
	}
	options.sharedChunkCache = nil
	options.sharedReaderCache = nil
	if options.cacheDir == nil || *options.cacheDir == "" {
		options.cacheDir = &directory
	}
	if options.wholeCacheSizeMB == nil || *options.wholeCacheSizeMB <= 0 {
		options.wholeCacheSizeMB = &wholeMB
	}
	return true
}
