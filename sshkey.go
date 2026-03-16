package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"strings"

	ig "github.com/yandex-cloud/go-genproto/yandex/cloud/compute/v1/instancegroup"
	"golang.org/x/crypto/ssh"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
)

// generateED25519Key generates an ephemeral ED25519 key pair and returns
// the PEM-encoded private key and the public key in authorized_keys format.
func generateED25519Key() (privateKeyPEM []byte, authorizedKey string, err error) {
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", fmt.Errorf("generating ed25519 key: %w", err)
	}

	pkcs8, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		return nil, "", fmt.Errorf("marshalling private key: %w", err)
	}

	privateKeyPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "PRIVATE KEY",
		Bytes: pkcs8,
	})

	sshPub, err := ssh.NewPublicKey(pubKey)
	if err != nil {
		return nil, "", fmt.Errorf("creating SSH public key: %w", err)
	}

	authorizedKey = strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
	return privateKeyPEM, authorizedKey, nil
}

// injectSSHKey updates the instance group template metadata to include the
// generated SSH public key. It preserves existing metadata entries and appends
// to any existing ssh-keys value.
//
// After the Update RPC succeeds the new template is accepted by YC and will
// be rolled out according to the group's deploy policy. The optional waitOp
// call is best-effort: a timeout or transient wait error does not mean the
// update was rejected, so we log a warning instead of failing. VMs that have
// not yet received the new key are reported as RUNNING_OUTDATED and handled
// by Update() (the provider method) which maps them to StateCreating.
func (g *InstanceGroup) injectSSHKey(ctx context.Context, group *ig.InstanceGroup) error {
	metadata := make(map[string]string)
	for k, v := range group.GetInstanceTemplate().GetMetadata() {
		metadata[k] = v
	}

	entry := g.SSHUser + ":" + g.sshPublicKey
	if existing, ok := metadata["ssh-keys"]; ok && existing != "" {
		// Replace any existing entry for the same SSH user to avoid
		// accumulating stale keys across plugin restarts.
		prefix := g.SSHUser + ":"
		var kept []string
		for _, line := range strings.Split(existing, "\n") {
			if line == "" || strings.HasPrefix(line, prefix) {
				continue
			}
			kept = append(kept, line)
		}
		kept = append(kept, entry)
		metadata["ssh-keys"] = strings.Join(kept, "\n")
	} else {
		metadata["ssh-keys"] = entry
	}

	op, err := g.client.Update(ctx, &ig.UpdateInstanceGroupRequest{
		InstanceGroupId: g.InstanceGroupID,
		UpdateMask: &fieldmaskpb.FieldMask{
			Paths: []string{"instance_template.metadata"},
		},
		InstanceTemplate: &ig.InstanceTemplate{
			Metadata: metadata,
		},
	})
	if err != nil {
		return fmt.Errorf("updating instance group metadata with SSH key: %w", err)
	}

	// Best-effort wait: if the wait fails the template is still accepted by
	// YC and will roll out. RUNNING_OUTDATED instances are already handled
	// by the Update provider method (mapped to StateCreating).
	if g.waitOp != nil {
		if err := g.waitOp(ctx, op); err != nil {
			g.log.Warn("could not confirm SSH key metadata rollout completed; "+
				"the template update was accepted and will proceed",
				"error", err)
		}
	}

	return nil
}
