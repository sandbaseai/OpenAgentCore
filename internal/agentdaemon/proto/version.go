package proto

// Version identifies the complete Core–Runtime wire contract. Change it when
// removing or changing required semantics. Additive extensions must explicitly
// negotiate support and retain the base contract when not selected.
const Version = "0.16.0"

// VersionCompatible accepts only this contract. Patch drift, prerelease suffixes
// and malformed versions do not select an implicit compatibility path.
func VersionCompatible(clientVersion string) bool {
	return clientVersion == Version
}
