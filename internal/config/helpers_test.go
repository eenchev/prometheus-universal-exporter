package config

// ptrTo is a pointer to a copy of v.
func ptrTo[T any](v T) *T { return &v }
