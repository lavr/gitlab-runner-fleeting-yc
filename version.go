package main

import "gitlab.com/gitlab-org/fleeting/fleeting/plugin"

var (
	// These are set via ldflags at build time.
	version   = "dev"
	revision  = "unknown"
	reference = "unknown"
	builtAt   = "unknown"
)

// Version is the plugin version info.
var Version = plugin.VersionInfo{
	Name:      "fleeting-plugin-yandexcloud",
	Version:   version,
	Revision:  revision,
	Reference: reference,
	BuiltAt:   builtAt,
}
