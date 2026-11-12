package wire

// A page does not compress what it sends, which is small: the compressor's tables are memory that a
// page's collector would then have to walk.
func deflate([]byte) []byte { return nil }
