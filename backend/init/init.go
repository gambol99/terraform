// Package backendinit is used to initialize backends.
//
// This package keeps track of the available backends and given a backend
// configuration will initialize the correct backend to use. This functionality
// is not in the "backend" package to avoid circular dependencies from backend
// implementations that need to use structs from the "backend" package.
//
// The returned backends from this package are ready to be used (State,
// Operation, etc.). All initialization functions such as Input, Validate,
// Configure will be called.
package backendinit
