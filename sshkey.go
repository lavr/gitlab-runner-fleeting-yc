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
func (g *InstanceGroup) injectSSHKey(ctx context.Context, group *ig.InstanceGroup) error {
	metadata := make(map[string]string)
	for k, v := range group.GetInstanceTemplate().GetMetadata() {
		metadata[k] = v
	}

	entry := g.SSHUser + ":" + g.sshPublicKey
	if existing, ok := metadata["ssh-keys"]; ok && existing != "" {
		metadata["ssh-keys"] = existing + "\n" + entry
	} else {
		metadata["ssh-keys"] = entry
	}

	_, err := g.client.Update(ctx, &ig.UpdateInstanceGroupRequest{
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

	return nil
}
