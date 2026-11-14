package cmd

import (
	"fmt"

	"github.com/spf13/viper"
)

// backupDestinations returns the effective raw destination specs and the base
// directory for relative paths. An explicit --backup-to list replaces the YAML
// array entirely; it is relative to the process working directory. YAML
// destinations are relative to the Git root.
func (opts *runOptions) backupDestinations(config *viper.Viper, projectRoot string) ([]string, string, error) {
	yamlSpecs, err := backupListFromConfig(config.Get("backup_to"))
	if err != nil {
		return nil, "", err
	}
	if opts.explicit["backup_to"] {
		return opts.backupTo, "", nil
	}
	return yamlSpecs, projectRoot, nil
}

// backupListFromConfig validates the YAML value even when an explicit flag will
// replace it, because a type error in the config file is still a config error.
func backupListFromConfig(raw any) ([]string, error) {
	if raw == nil {
		return nil, nil
	}

	specs := make([]string, 0)
	switch value := raw.(type) {
	case []string:
		specs = append(specs, value...)
	case []any:
		for i, item := range value {
			spec, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("backup_to[%d] must be a string", i)
			}
			specs = append(specs, spec)
		}
	default:
		return nil, fmt.Errorf("backup_to must be a list of strings")
	}
	return specs, nil
}
