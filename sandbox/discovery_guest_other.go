//go:build !linux

package sandbox

// Remote helper installation is unavailable on unsupported host platforms.
type guestDiscovery struct{}
