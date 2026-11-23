package config

import (
	"errors"

	"github.com/eenchev/prometheus-universal-exporter/internal/fetch"
	"github.com/eenchev/prometheus-universal-exporter/internal/model"
)

// validateSecretStores checks secret_stores and every reference to one in
// the configuration (fetch/secretstore.go): a key that names a credential
// file may hold a reference to a store the configuration has, and no other
// key may. The exporter's own credentials, web.basic_auth's, are compared
// at every request it answers, from files it stats, and take none.
func validateSecretStores(c *model.Config) error {
	if err := fetch.ValidateSecretStores(c.SecretStores); err != nil {
		return err
	}
	if a := c.Web.BasicAuth; a != nil && (fetch.IsSecretReference(a.UsernameFile) || fetch.IsSecretReference(a.PasswordFile)) {
		return errors.New("web.basic_auth names a secret store for username_file or password_file; the exporter's own credentials are read from files, and a secret store is for the credentials it sends its targets")
	}
	return fetch.NewSecretReferences(c.SecretStores).Check("the configuration", "", c)
}
