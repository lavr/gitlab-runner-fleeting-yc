package main

import "fmt"

const defaultGroupName = "fleeting-plugin-yandexcloud"

// Config holds the plugin configuration deserialized from plugin_config in config.toml.
type Config struct {
	// FolderID is the Yandex Cloud folder containing the instance group.
	FolderID string `json:"folder_id"`

	// InstanceGroupID is the ID of the instance group to manage.
	// Mutually exclusive with TemplateFile.
	InstanceGroupID string `json:"instance_group_id,omitempty"`

	// TemplateFile is the path to a YAML template for creating an instance group.
	// Mutually exclusive with InstanceGroupID.
	TemplateFile string `json:"template_file,omitempty"`

	// DeleteOnShutdown controls whether a plugin-created group is deleted on Shutdown.
	DeleteOnShutdown bool `json:"delete_on_shutdown,omitempty"`

	// GroupName is the name used to find or create the instance group for idempotency.
	// Defaults to "fleeting-plugin-yandexcloud".
	GroupName string `json:"group_name,omitempty"`

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
	if c.InstanceGroupID != "" && c.TemplateFile != "" {
		return fmt.Errorf("instance_group_id and template_file are mutually exclusive")
	}
	if c.InstanceGroupID == "" && c.TemplateFile == "" {
		return fmt.Errorf("one of instance_group_id or template_file is required")
	}
	if c.DeleteOnShutdown && c.TemplateFile == "" {
		return fmt.Errorf("delete_on_shutdown requires template_file (has no effect with instance_group_id)")
	}
	if c.GroupName == "" {
		c.GroupName = defaultGroupName
	}
	if c.SSHUser == "" {
		c.SSHUser = "ubuntu"
	}
	return nil
}
