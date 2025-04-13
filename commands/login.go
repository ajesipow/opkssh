// Copyright 2025 OpenPubkey
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package commands

import (
	"context"
	"crypto"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lestrrat-go/jwx/v2/jwa"
	"github.com/lestrrat-go/jwx/v2/jws"
	"github.com/openpubkey/openpubkey/client"
	"github.com/openpubkey/openpubkey/client/choosers"
	"github.com/openpubkey/openpubkey/oidc"
	"github.com/openpubkey/openpubkey/pktoken"
	"github.com/openpubkey/openpubkey/providers"
	"github.com/openpubkey/openpubkey/util"
	"github.com/openpubkey/opkssh/sshcert"
	"golang.org/x/crypto/ssh"
)

var DefaultProviderList = "google,https://accounts.google.com,206584157355-7cbe4s640tvm7naoludob4ut1emii7sf.apps.googleusercontent.com,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;" +
	"microsoft,https://login.microsoftonline.com/9188040d-6c67-4c5b-b112-36a304b66dad/v2.0,096ce0a3-5e72-4da8-9c86-12924b294a01;" +
	"gitlab,https://gitlab.com,8d8b7024572c7fd501f64374dec6bba37096783dfcd792b3988104be08cb6923"

type LoginCmd struct {
	autoRefresh           bool
	logDir                string
	disableBrowserOpenArg bool
	printIdTokenArg       bool
	keyPathArg            string
	providerArg           string
	providerFromLdFlags   providers.OpenIdProvider
	providerAlias         string
	rcFilePath            string // file path for the config file ~/.opksshrc
	pkt                   *pktoken.PKToken
	signer                crypto.Signer
	alg                   jwa.SignatureAlgorithm
	client                *client.OpkClient
	principals            []string
}

func NewLogin(autoRefresh bool, logDir string, disableBrowserOpenArg bool, printIdTokenArg bool, providerArg string, keyPathArg string, providerFromLdFlags providers.OpenIdProvider, providerAlias string) *LoginCmd {
	rcFilePath := ""
	if homePath, err := os.UserHomeDir(); err != nil {
		log.Printf("Failed to get home directory: %v \n", err)
	} else {
		rcFilePath = filepath.Join(homePath, ".opksshrc")
	}

	return &LoginCmd{
		autoRefresh:           autoRefresh,
		logDir:                logDir,
		disableBrowserOpenArg: disableBrowserOpenArg,
		printIdTokenArg:       printIdTokenArg,
		keyPathArg:            keyPathArg,
		providerArg:           providerArg,
		providerFromLdFlags:   providerFromLdFlags,
		providerAlias:         providerAlias,
		rcFilePath:            rcFilePath,
	}
}

func (l *LoginCmd) Run(ctx context.Context) error {
	// If a log directory was provided, write any logs to a file in that directory AND stdout
	if l.logDir != "" {
		logFilePath := filepath.Join(l.logDir, "opkssh.log")
		logFile, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0660)
		if err != nil {
			log.Printf("Failed to open log for writing: %v \n", err)
		}
		defer logFile.Close()
		multiWriter := io.MultiWriter(os.Stdout, logFile)
		log.SetOutput(multiWriter)
	} else {
		log.SetOutput(os.Stdout)
	}

	openBrowser := !l.disableBrowserOpenArg

	// If the user has supplied commandline arguments for the provider, use those instead of the web chooser
	var provider providers.OpenIdProvider
	if l.providerArg != "" {
		config, err := NewProviderConfigFromString(l.providerArg, false)
		if err != nil {
			return fmt.Errorf("error parsing provider argument: %w", err)
		}

		provider, err = NewProviderFromConfig(config, openBrowser)

		if err != nil {
			return fmt.Errorf("error creating provider from config: %w", err)
		}
	} else if l.providerFromLdFlags != nil {
		provider = l.providerFromLdFlags
	} else {
		var err error
		if _, ok := os.LookupEnv("OPKSSH_PROVIDERS"); !ok {
			if _, err := os.Stat(l.rcFilePath); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					log.Println("No config file found at ", l.rcFilePath)
				} else {
					return fmt.Errorf("error checking for config file: %w", err)
				}
			} else {
				err = SetEnvFromConfigFile(l.rcFilePath)
				if err != nil {
					return fmt.Errorf("error setting env from config file: %w", err)
				}
			}
		}

		// Get the default provider from the env variable
		defaultProvider, ok := os.LookupEnv("OPKSSH_DEFAULT")
		if !ok {
			defaultProvider = "WEBCHOOSER"
		}
		providerConfigs, err := GetProvidersConfigFromEnv()

		if err != nil {
			return fmt.Errorf("error getting provider config from env: %w", err)
		}

		if l.providerAlias != "" && l.providerAlias != "WEBCHOOSER" {
			config, ok := providerConfigs[l.providerAlias]
			if !ok {
				return fmt.Errorf("error getting provider config for alias %s", l.providerAlias)
			}
			provider, err = NewProviderFromConfig(config, openBrowser)
			if err != nil {
				return fmt.Errorf("error creating provider from config: %w", err)
			}
		} else {
			if defaultProvider != "WEBCHOOSER" {
				config, ok := providerConfigs[defaultProvider]
				if !ok {
					return fmt.Errorf("error getting provider config for alias %s", defaultProvider)
				}
				provider, err = NewProviderFromConfig(config, openBrowser)
				if err != nil {
					return fmt.Errorf("error creating provider from config: %w", err)
				}
			} else {
				var idpList []providers.BrowserOpenIdProvider
				for _, config := range providerConfigs {
					provider, err := NewProviderFromConfig(config, openBrowser)
					if err != nil {
						return fmt.Errorf("error creating provider from config: %w", err)
					}
					idpList = append(idpList, provider.(providers.BrowserOpenIdProvider))
				}

				provider, err = choosers.NewWebChooser(
					idpList, openBrowser,
				).ChooseOp(ctx)
				if err != nil {
					return fmt.Errorf("error selecting OpenID provider: %w", err)
				}
			}
		}
	}

	// Execute login command
	if l.autoRefresh {
		if providerRefreshable, ok := provider.(providers.RefreshableOpenIdProvider); ok {
			err := LoginWithRefresh(ctx, providerRefreshable, l.printIdTokenArg, l.keyPathArg)
			if err != nil {
				return fmt.Errorf("error logging in: %w", err)
			}
		} else {
			return fmt.Errorf("supplied OpenID Provider (%v) does not support auto-refresh and auto-refresh argument set to true", provider.Issuer())
		}
	} else {
		err := Login(ctx, provider, l.printIdTokenArg, l.keyPathArg)
		if err != nil {
			return fmt.Errorf("error logging in: %w", err)
		}
	}
	return nil
}

func login(ctx context.Context, provider client.OpenIdProvider, printIdToken bool, seckeyPath string) (*LoginCmd, error) {
	var err error
	alg := jwa.ES256
	signer, err := util.GenKeyPair(alg)
	if err != nil {
		return nil, fmt.Errorf("failed to generate keypair: %w", err)
	}

	opkClient, err := client.New(provider, client.WithSigner(signer, alg))
	if err != nil {
		return nil, err
	}

	pkt, err := opkClient.Auth(ctx)
	if err != nil {
		return nil, err
	}

	// If principals is empty the server does not enforce any principal. The OPK
	// verifier should use policy to make this decision.
	principals := []string{}
	certBytes, seckeySshPem, err := createSSHCert(pkt, signer, principals)
	if err != nil {
		return nil, fmt.Errorf("failed to generate SSH cert: %w", err)
	}

	// Write ssh secret key and public key to filesystem
	if seckeyPath != "" {
		// If we have set seckeyPath then write it there
		if err := writeKeys(seckeyPath, seckeyPath+".pub", seckeySshPem, certBytes); err != nil {
			return nil, fmt.Errorf("failed to write SSH keys to filesystem: %w", err)
		}
	} else {
		// If keyPath isn't set then write it to the default location
		if err := writeKeysToSSHDir(seckeySshPem, certBytes); err != nil {
			return nil, fmt.Errorf("failed to write SSH keys to filesystem: %w", err)
		}
	}

	if printIdToken {
		idTokenStr, err := PrettyIdToken(*pkt)

		if err != nil {
			return nil, fmt.Errorf("failed to format ID Token: %w", err)
		}

		fmt.Printf("id_token:\n%s\n", idTokenStr)
	}

	idStr, err := IdentityString(*pkt)
	if err != nil {
		return nil, fmt.Errorf("failed to parse ID Token: %w", err)
	}
	fmt.Printf("Keys generated for identity\n%s\n", idStr)

	return &LoginCmd{
		pkt:        pkt,
		signer:     signer,
		client:     opkClient,
		alg:        alg,
		principals: principals,
	}, nil
}

// Login performs the OIDC login procedure and creates the SSH certs/keys in the
// default SSH key location.
func Login(ctx context.Context, provider client.OpenIdProvider, printIdToken bool, seckeyPath string) error {
	_, err := login(ctx, provider, printIdToken, seckeyPath)
	return err
}

// LoginWithRefresh performs the OIDC login procedure, creates the SSH
// certs/keys in the default SSH key location, and continues to run and refresh
// the PKT (and create new SSH certs) indefinitely as its token expires. This
// function only returns if it encounters an error or if the supplied context is
// cancelled.
func LoginWithRefresh(ctx context.Context, provider providers.RefreshableOpenIdProvider, printIdToken bool, seckeyPath string) error {
	if loginResult, err := login(ctx, provider, printIdToken, seckeyPath); err != nil {
		return err
	} else {
		var claims struct {
			Expiration int64 `json:"exp"`
		}
		if err := json.Unmarshal(loginResult.pkt.Payload, &claims); err != nil {
			return err
		}

		for {
			// Sleep until a minute before expiration to give us time to refresh
			// the token and minimize any interruptions
			untilExpired := time.Until(time.Unix(claims.Expiration, 0)) - time.Minute
			log.Printf("Waiting for %v before attempting to refresh id_token...", untilExpired)
			select {
			case <-time.After(untilExpired):
				log.Print("Refreshing id_token...")
			case <-ctx.Done():
				return ctx.Err()
			}

			refreshedPkt, err := loginResult.client.Refresh(ctx)
			if err != nil {
				return err
			}
			loginResult.pkt = refreshedPkt

			certBytes, seckeySshPem, err := createSSHCert(loginResult.pkt, loginResult.signer, loginResult.principals)
			if err != nil {
				return fmt.Errorf("failed to generate SSH cert: %w", err)
			}

			// Write ssh secret key and public key to filesystem
			if seckeyPath != "" {
				// If we have set seckeyPath then write it there
				if err := writeKeys(seckeyPath, seckeyPath+".pub", seckeySshPem, certBytes); err != nil {
					return fmt.Errorf("failed to write SSH keys to filesystem: %w", err)
				}
			} else {
				// If keyPath isn't set then write it to the default location
				if err := writeKeysToSSHDir(seckeySshPem, certBytes); err != nil {
					return fmt.Errorf("failed to write SSH keys to filesystem: %w", err)
				}
			}

			comPkt, err := refreshedPkt.Compact()
			if err != nil {
				return err
			}

			_, payloadB64, _, err := jws.SplitCompactString(string(comPkt))
			if err != nil {
				return fmt.Errorf("malformed ID token: %w", err)
			}
			payload, err := base64.RawURLEncoding.DecodeString(string(payloadB64))
			if err != nil {
				return fmt.Errorf("refreshed ID token payload is not base64 encoded: %w", err)
			}

			if err = json.Unmarshal(payload, &claims); err != nil {
				return fmt.Errorf("malformed refreshed ID token payload: %w", err)
			}
		}
	}
}

func createSSHCert(pkt *pktoken.PKToken, signer crypto.Signer, principals []string) ([]byte, []byte, error) {
	cert, err := sshcert.New(pkt, principals)
	if err != nil {
		return nil, nil, err
	}
	sshSigner, err := ssh.NewSignerFromSigner(signer)
	if err != nil {
		return nil, nil, err
	}

	signerMas, err := ssh.NewSignerWithAlgorithms(sshSigner.(ssh.AlgorithmSigner), []string{ssh.KeyAlgoECDSA256})
	if err != nil {
		return nil, nil, err
	}

	sshCert, err := cert.SignCert(signerMas)
	if err != nil {
		return nil, nil, err
	}
	certBytes := ssh.MarshalAuthorizedKey(sshCert)
	// Remove newline character that MarshalAuthorizedKey() adds
	certBytes = certBytes[:len(certBytes)-1]

	seckeySsh, err := ssh.MarshalPrivateKey(signer, "openpubkey cert")
	if err != nil {
		return nil, nil, err
	}
	seckeySshBytes := pem.EncodeToMemory(seckeySsh)

	return certBytes, seckeySshBytes, nil
}

func writeKeysToSSHDir(seckeySshPem []byte, certBytes []byte) error {
	homePath, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	sshPath := filepath.Join(homePath, ".ssh")

	// Make ~/.ssh if folder does not exist
	err = os.MkdirAll(sshPath, os.ModePerm)
	if err != nil {
		return err
	}

	// For ssh to automatically find the key created by openpubkey when
	// connecting, we use one of the default ssh key paths. However, the file
	// might contain an existing key. We will overwrite the key if it was
	// generated by openpubkey  which we check by looking at the associated
	// comment. If the comment is equal to "openpubkey", we overwrite the file
	// with a new key.
	for _, keyFilename := range []string{"id_ecdsa", "id_ed25519"} {
		seckeyPath := filepath.Join(sshPath, keyFilename)
		pubkeyPath := seckeyPath + ".pub"

		if !fileExists(seckeyPath) {
			// If ssh key file does not currently exist, we don't have to worry about overwriting it
			return writeKeys(seckeyPath, pubkeyPath, seckeySshPem, certBytes)
		} else if !fileExists(pubkeyPath) {
			continue
		} else {
			// If the ssh key file does exist, check if it was generated by openpubkey, if it was then it is safe to overwrite
			sshPubkey, err := os.ReadFile(pubkeyPath)
			if err != nil {
				log.Println("Failed to read:", pubkeyPath)
				continue
			}
			_, comment, _, _, err := ssh.ParseAuthorizedKey(sshPubkey)
			if err != nil {
				log.Println("Failed to parse:", pubkeyPath)
				continue
			}

			// If the key comment is "openpubkey" then we generated it
			if comment == "openpubkey" {
				return writeKeys(seckeyPath, pubkeyPath, seckeySshPem, certBytes)
			}
		}
	}
	return fmt.Errorf("no default ssh key file free for openpubkey")
}

func writeKeys(seckeyPath string, pubkeyPath string, seckeySshPem []byte, certBytes []byte) error {
	// Write ssh secret key to filesystem
	if err := os.WriteFile(seckeyPath, seckeySshPem, 0600); err != nil {
		return err
	}

	fmt.Printf("Writing opk ssh public key to %s and corresponding secret key to %s\n", pubkeyPath, seckeyPath)

	certBytes = append(certBytes, []byte(" openpubkey")...)
	// Write ssh public key (certificate) to filesystem
	return os.WriteFile(pubkeyPath, certBytes, 0644)
}

func fileExists(fPath string) bool {
	_, err := os.Open(fPath)
	return !errors.Is(err, os.ErrNotExist)
}

func IdentityString(pkt pktoken.PKToken) (string, error) {
	idt, err := oidc.NewJwt(pkt.OpToken)
	if err != nil {
		return "", err
	}
	claims := idt.GetClaims()
	if claims.Email == "" {
		return "Sub, issuer, audience: \n" + claims.Subject + " " + claims.Issuer + " " + claims.Audience, nil
	} else {
		return "Email, sub, issuer, audience: \n" + claims.Email + " " + claims.Subject + " " + claims.Issuer + " " + claims.Audience, nil
	}
}

// ProviderConfig is the representation of the provider config:
// {alias},{provider_url},{client_id},{client_secret},{scopes}
// client secret is optional, as well as scopes, if not provided, the default for secret is an empty string, for scopes is "openid profile email"
type ProviderConfig struct {
	Alias        string
	Issuer       string
	ClientID     string
	ClientSecret string
	Scopes       []string
}

// NewProviderConfigFromString is a function to create the provider config from a string of the format
// {alias},{provider_url},{client_id},{client_secret},{scopes}
func NewProviderConfigFromString(configStr string, hasAlias bool) (ProviderConfig, error) {
	parts := strings.Split(configStr, ",")
	alias := ""
	if hasAlias {
		// If the config string has an alias, we need to remove it from the parts
		alias = parts[0]
		parts = parts[1:]
	}
	if len(parts) < 2 {
		if hasAlias {
			return ProviderConfig{}, fmt.Errorf("invalid provider config string. Expected format <alias>,<issuer>,<client_id> or <alias>,<issuer>,<client_id>,<client_secret> or <alias>,<issuer>,<client_id>,<client_secret>,<scopes>")
		}
		return ProviderConfig{}, fmt.Errorf("invalid provider config string. Expected format <issuer>,<client_id> or <issuer>,<client_id>,<client_secret> or <issuer>,<client_id>,<client_secret>,<scopes>")
	}

	providerConfig := ProviderConfig{
		Alias:    alias,
		Issuer:   parts[0],
		ClientID: parts[1],
	}

	if providerConfig.ClientID == "" {
		return ProviderConfig{}, fmt.Errorf("invalid provider client-ID value got (%s)", providerConfig.ClientID)
	}

	if len(parts) > 2 {
		providerConfig.ClientSecret = parts[2]
	} else {
		providerConfig.ClientSecret = ""
	}

	if len(parts) > 3 {
		providerConfig.Scopes = strings.Split(parts[3], " ")
	} else {
		providerConfig.Scopes = []string{"openid", "profile", "email"}
	}

	if strings.HasPrefix(providerConfig.Issuer, "https://accounts.google.com") {
		// The Google OP is strange in that it requires a client secret even if this is a public OIDC App.
		// Despite its name the Google OP client secret is a public value.
		if providerConfig.ClientSecret == "" {
			if hasAlias {
				return ProviderConfig{}, fmt.Errorf("invalid provider argument format. Expected format for google: <alias>,<issuer>,<client_id>,<client_secret>")
			} else {
				return ProviderConfig{}, fmt.Errorf("invalid provider argument format. Expected format for google: <issuer>,<client_id>,<client_secret>")
			}

		}
	}
	return providerConfig, nil
}

// NewProviderFromConfig is a function to create the provider from the config
func NewProviderFromConfig(config ProviderConfig, openBrowser bool) (client.OpenIdProvider, error) {

	if config.Issuer == "" {
		return nil, fmt.Errorf("invalid provider issuer value got (%s)", config.Issuer)
	}

	if !strings.HasPrefix(config.Issuer, "https://") {
		return nil, fmt.Errorf("invalid provider issuer value. Expected issuer to start with 'https://' got (%s)", config.Issuer)
	}

	if config.ClientID == "" {
		return nil, fmt.Errorf("invalid provider client-ID value got (%s)", config.ClientID)
	}
	var provider client.OpenIdProvider

	if strings.HasPrefix(config.Issuer, "https://accounts.google.com") {
		opts := providers.GetDefaultGoogleOpOptions()
		opts.Issuer = config.Issuer
		opts.ClientID = config.ClientID
		opts.ClientSecret = config.ClientSecret
		opts.GQSign = false
		opts.OpenBrowser = openBrowser
		provider = providers.NewGoogleOpWithOptions(opts)
	} else if strings.HasPrefix(config.Issuer, "https://login.microsoftonline.com") {
		opts := providers.GetDefaultAzureOpOptions()
		opts.Issuer = config.Issuer
		opts.ClientID = config.ClientID
		opts.GQSign = false
		opts.OpenBrowser = openBrowser
		provider = providers.NewAzureOpWithOptions(opts)
	} else if strings.HasPrefix(config.Issuer, "https://gitlab.com") {
		opts := providers.GetDefaultGitlabOpOptions()
		opts.Issuer = config.Issuer
		opts.ClientID = config.ClientID
		opts.GQSign = false
		opts.OpenBrowser = openBrowser
		provider = providers.NewGitlabOpWithOptions(opts)
	} else {
		// Generic provider - Need signing, no encryption
		opts := providers.GetDefaultGoogleOpOptions()
		opts.Issuer = config.Issuer
		opts.ClientID = config.ClientID
		opts.GQSign = false
		opts.ClientSecret = config.ClientSecret
		opts.Scopes = config.Scopes
		opts.OpenBrowser = openBrowser

		provider = providers.NewGoogleOpWithOptions(opts)
	}

	return provider, nil
}

// GetProvidersConfigFromEnv is a function to retrieve the config from the env variables
// OPKSSH_DEFAULT can be set to an alias
// OPKSSH_PROVIDERS is a ; separated list of providers of the format <alias>,<issuer>,<client_id>,<client_secret>,<scopes>;<alias>,<issuer>,<client_id>,<client_secret>,<scopes>
func GetProvidersConfigFromEnv() (map[string]ProviderConfig, error) {
	providersConfig := make(map[string]ProviderConfig)

	// Get the providers from the env variable
	providerList, ok := os.LookupEnv("OPKSSH_PROVIDERS")
	if !ok {
		providerList = DefaultProviderList
	}

	for _, providerStr := range strings.Split(providerList, ";") {
		config, err := NewProviderConfigFromString(providerStr, true)
		if err != nil {
			return nil, fmt.Errorf("error parsing provider config string: %w", err)
		}
		// If alias already exists, return an error
		if _, ok := providersConfig[config.Alias]; ok {
			return nil, fmt.Errorf("duplicate provider alias found: %s", config.Alias)
		}
		providersConfig[config.Alias] = config
	}

	return providersConfig, nil
}

// GetEnvFromConfigFile is a function to retrieve the env variables from the ~/.opksshrc file
func GetEnvFromConfigFile(rcPath string) (map[string]string, error) {
	if _, err := os.Stat(rcPath); errors.Is(err, os.ErrNotExist) {
		envs := map[string]string{
			"OPKSSH_DEFAULT":   "WEBCHOOSER",
			"OPKSSH_PROVIDERS": DefaultProviderList,
		}
		fileContent := ""
		for k, v := range envs {
			fileContent += fmt.Sprintf("%s=%s\n", k, v)
		}
		if err := os.WriteFile(rcPath, []byte(fileContent), 0600); err != nil {
			return nil, err
		}
		return envs, nil
	}

	fileContent, err := os.ReadFile(rcPath)
	if err != nil {
		return nil, err
	}

	envs := make(map[string]string)
	lines := strings.Split(string(fileContent), "\n")
	// For each line we need to parse the env variable, if it references itself, we need to handle it, if it reference another env variable, we need to handle it too
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}

		envs[parts[0]] = os.Expand(parts[1], func(key string) string {
			// If in envs variable, return the value
			if val, ok := envs[key]; ok {
				return val
			}
			// If in os env, return the value
			if val, ok := os.LookupEnv(key); ok {
				return val
			}
			return ""
		})
	}

	return envs, nil
}

// SetEnvFromConfigFile is a function to set the env variables from the ~/.opksshrc file. This does not overwrite existing env variables.
func SetEnvFromConfigFile(rcPath string) error {
	envs, err := GetEnvFromConfigFile(rcPath)

	if err != nil {
		return err
	}
	for k, v := range envs {
		if os.Getenv(k) != "" {
			continue
		} else if err := os.Setenv(k, v); err != nil {
			return err
		}
	}
	return nil
}

func PrettyIdToken(pkt pktoken.PKToken) (string, error) {
	idt, err := oidc.NewJwt(pkt.OpToken)
	if err != nil {
		return "", err
	}
	idtJson, err := json.MarshalIndent(idt.GetClaims(), "", "    ")

	if err != nil {
		return "", err
	}
	return string(idtJson[:]), nil
}
