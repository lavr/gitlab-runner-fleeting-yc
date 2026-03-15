package main

import "fmt"

// Config holds the plugin configuration deserialized from plugin_config in config.toml.
type Config struct {
	// FolderID is the Yandex Cloud folder containing the instance group.
	FolderID string `json:"folder_id"`

	// InstanceGroupID is the ID of the instance group to manage.
	InstanceGroupID string `json:"instance_group_id"`

	// KeyFile is the path to a JSON IAM key file for authentication.
	// If empty, the plugin falls back to the metadata service (instance service account).
	KeyFile string `json:"key_file,omitempty"`

	// SSHUser is the username for SSH connections. Defaults to "ubuntu".
	SSHUser string `json:"ssh_user,omitempty"`
}

// validate checks that required fields are set and applies defaults.
func (c *Config) validate() error {
	if c.FolderID == "" {
		return fmt.Errorf("folder_id is required")
	}
	if c.InstanceGroupID == "" {
		return fmt.Errorf("instance_group_id is required")
	}
	if c.SSHUser == "" {
		c.SSHUser = "ubuntu"
	}
	return nil
}
