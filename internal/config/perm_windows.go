package config

// warnInsecurePermissions is a no-op on Windows. os.FileMode.Perm() returns
// a synthetic 0666 on Windows regardless of actual ACLs, so Unix-style
// permission checks produce false positives.
func warnInsecurePermissions(_ string) {}
