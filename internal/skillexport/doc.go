// Package skillexport renders the "get packages from this host's private
// registries" skill for an in-house AI agent.
//
// The exported content is not a fixed brochure: registry URLs come from the
// host entry and the package lists come from the verified inventory produced by
// apply/verify. That is the point — an offline agent told to use a package the
// registry does not serve has no way to recover on its own.
//
// Layout written under OutDir:
//
//	SKILL.md
//	references/packages.md
//	references/troubleshooting.md
//
// Templates live in templates/ and are embedded at build time, so the CLI stays
// a single self-contained binary.
package skillexport
