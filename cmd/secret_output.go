package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/geektr-cloud/openmgcl/internal/tacos"
	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"
)

type secretListView struct {
	Name      string `json:"name" yaml:"name"`
	CreatedAt string `json:"created_at" yaml:"created_at"`
	UpdatedAt string `json:"updated_at" yaml:"updated_at"`
	KeyCount  int    `json:"key_count" yaml:"key_count"`
}

type secretKeyView struct {
	Key         string `json:"key" yaml:"key"`
	CreatedAt   string `json:"created_at" yaml:"created_at"`
	UpdatedAt   string `json:"updated_at" yaml:"updated_at"`
	IsEphemeral bool   `json:"is_ephemeral" yaml:"is_ephemeral"`
}

type secretDetailsView struct {
	ID        string          `json:"id" yaml:"id"`
	Name      string          `json:"name" yaml:"name"`
	CreatedAt string          `json:"created_at" yaml:"created_at"`
	UpdatedAt string          `json:"updated_at" yaml:"updated_at"`
	Keys      []secretKeyView `json:"keys" yaml:"keys"`
}

func secretAccessLabel(isEphemeral bool) string {
	if isEphemeral {
		return "ephemeral"
	}
	return "public"
}

func writeSecretList(cmd *cobra.Command, secrets []tacos.SecretSummary) error {
	out := cmd.OutOrStdout()
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tCREATED\tUPDATED\tKEYS")
	for _, secret := range secrets {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\n", secret.Name, secret.CreatedAt, secret.UpdatedAt, secret.KeyCount)
	}
	return w.Flush()
}

func writeSecretDetails(cmd *cobra.Command, details *tacos.SecretDetails, format string) error {
	switch format {
	case "table", "":
		return writeSecretDetailsTable(cmd.OutOrStdout(), details)
	case "json":
		view := secretDetailsToView(details)
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(view); err != nil {
			return err
		}
		return nil
	case "yaml":
		view := secretDetailsToView(details)
		data, err := yaml.Marshal(view)
		if err != nil {
			return err
		}
		_, err = cmd.OutOrStdout().Write(data)
		return err
	default:
		return fmt.Errorf("unsupported output format %q (want table, yaml, or json)", format)
	}
}

func secretDetailsToView(details *tacos.SecretDetails) secretDetailsView {
	keys := make([]secretKeyView, 0, len(details.Keys))
	for _, key := range details.Keys {
		keys = append(keys, secretKeyView{
			Key:         key.Key,
			CreatedAt:   key.CreatedAt,
			UpdatedAt:   key.UpdatedAt,
			IsEphemeral: key.IsEphemeral,
		})
	}
	return secretDetailsView{
		ID:        details.ID,
		Name:      details.Name,
		CreatedAt: details.CreatedAt,
		UpdatedAt: details.UpdatedAt,
		Keys:      keys,
	}
}

func writeSecretDetailsTable(out io.Writer, details *tacos.SecretDetails) error {
	fmt.Fprintf(out, "ID: %s\n", details.ID)
	fmt.Fprintf(out, "Name: %s\n", details.Name)
	fmt.Fprintf(out, "Created: %s\n", details.CreatedAt)
	fmt.Fprintf(out, "Updated: %s\n", details.UpdatedAt)
	if len(details.Keys) == 0 {
		fmt.Fprintln(out, "Keys: (none)")
		return nil
	}
	fmt.Fprintln(out, "Keys:")
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "  KEY\tCREATED\tUPDATED\tACCESS")
	for _, key := range details.Keys {
		fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", key.Key, key.CreatedAt, key.UpdatedAt, secretAccessLabel(key.IsEphemeral))
	}
	return w.Flush()
}
