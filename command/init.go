package command

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	consulapi "github.com/hashicorp/consul/api"
	"github.com/hashicorp/vault/api"
	"github.com/hashicorp/vault/helper/pgpkeys"
	"github.com/hashicorp/vault/meta"
)

// InitCommand is a Command that initializes a new Vault server.
type InitCommand struct {
	meta.Meta
}

func (c *InitCommand) Run(args []string) int {
	var threshold, shares, storedShares, recoveryThreshold, recoveryShares int
	var pgpKeys, recoveryPgpKeys pgpkeys.PubKeyFilesFlag
	var check bool
	var auto string
	flags := c.Meta.FlagSet("init", meta.FlagSetDefault)
	flags.Usage = func() { c.Ui.Error(c.Help()) }
	flags.IntVar(&shares, "key-shares", 5, "")
	flags.IntVar(&threshold, "key-threshold", 3, "")
	flags.IntVar(&storedShares, "stored-shares", 0, "")
	flags.Var(&pgpKeys, "pgp-keys", "")
	flags.IntVar(&recoveryShares, "recovery-shares", 5, "")
	flags.IntVar(&recoveryThreshold, "recovery-threshold", 3, "")
	flags.Var(&recoveryPgpKeys, "recovery-pgp-keys", "")
	flags.BoolVar(&check, "check", false, "")
	flags.StringVar(&auto, "auto", "", "")
	if err := flags.Parse(args); err != nil {
		return 1
	}

	initRequest := &api.InitRequest{
		SecretShares:      shares,
		SecretThreshold:   threshold,
		StoredShares:      storedShares,
		PGPKeys:           pgpKeys,
		RecoveryShares:    recoveryShares,
		RecoveryThreshold: recoveryThreshold,
		RecoveryPGPKeys:   recoveryPgpKeys,
	}

	// If running in 'auto' mode, run service discovery based on environment
	// variables of Consul.
	if auto != "" {
		// Create configuration for Consul
		consulConfig := consulapi.DefaultConfig()

		// Create a client to communicate with Consul
		consulClient, err := consulapi.NewClient(consulConfig)
		if err != nil {
			c.Ui.Error(fmt.Sprintf("failed to create Consul client:%v", err))
			return 1
		}

		var uninitializedVaults []string
		var initializedVault string

		// Query the nodes belonging to the cluster
		if services, _, err := consulClient.Catalog().Service(auto, "", &consulapi.QueryOptions{AllowStale: true}); err == nil {
		Loop:
			for _, service := range services {
				vaultAddress := fmt.Sprintf("%s://%s:%d", consulConfig.Scheme, service.ServiceAddress, service.ServicePort)

				// Set VAULT_ADDR to the discovered node
				os.Setenv(api.EnvVaultAddress, vaultAddress)

				// Create a client to communicate with the discovered node
				client, err := c.Client()
				if err != nil {
					c.Ui.Error(fmt.Sprintf(
						"Error initializing client: %s", err))
					return 1
				}

				// Check the initialization status of the discovered node
				inited, err := client.Sys().InitStatus()
				switch {
				case err != nil:
					c.Ui.Error(fmt.Sprintf("Error checking initialization status of discovered node: %s err:%s", vaultAddress, err))
					return 1
				case inited:
					// One of the nodes in the cluster is initialized. Break out.
					initializedVault = vaultAddress
					break Loop
				default:
					// Vault is uninitialized.
					uninitializedVaults = append(uninitializedVaults, vaultAddress)
				}
			}
		}

		export := "export"
		quote := "'"
		if runtime.GOOS == "windows" {
			export = "set"
			quote = ""
		}

		if initializedVault != "" {
			c.Ui.Output(fmt.Sprintf("Discovered an initialized Vault node at '%s'\n", initializedVault))
			c.Ui.Output("Set the following environment variable to operate on the discovered Vault:\n")
			c.Ui.Output(fmt.Sprintf("\t%s VAULT_ADDR=%shttp://%s%s", export, quote, initializedVault, quote))
			return 0
		}

		switch len(uninitializedVaults) {
		case 0:
			c.Ui.Error(fmt.Sprintf("Failed to discover Vault nodes under the service name '%s'", auto))
			return 1
		case 1:
			// There was only one node found in the Vault cluster and it
			// was uninitialized.

			// Set the VAULT_ADDR to the discovered node. This will ensure
			// that the client created will operate on the discovered node.
			os.Setenv(api.EnvVaultAddress, uninitializedVaults[0])

			// Let the client know that initialization is perfomed on the
			// discovered node.
			c.Ui.Output(fmt.Sprintf("Discovered Vault at '%s'\n", uninitializedVaults[0]))

			// Attempt initializing it
			ret := c.runInit(check, initRequest)

			// Regardless of success or failure, instruct client to update VAULT_ADDR
			c.Ui.Output("Set the following environment variable to operate on the discovered Vault:\n")
			c.Ui.Output(fmt.Sprintf("\t%s VAULT_ADDR=%shttp://%s%s", export, quote, uninitializedVaults[0], quote))

			return ret
		default:
			// If more than one Vault node were discovered, print out all of them,
			// requiring the client to update VAULT_ADDR and to run init again.
			c.Ui.Output(fmt.Sprintf("Discovered more than one uninitialized Vaults under the service name '%s'\n", auto))
			c.Ui.Output("To initialize all Vaults, set any *one* of the following and run 'vault init':")

			// Print valid commands to make setting the variables easier
			for _, vaultNode := range uninitializedVaults {
				c.Ui.Output(fmt.Sprintf("\t%s VAULT_ADDR=%shttp://%s%s", export, quote, vaultNode, quote))

			}
			return 0
		}
	}

	return c.runInit(check, initRequest)
}

func (c *InitCommand) runInit(check bool, initRequest *api.InitRequest) int {
	client, err := c.Client()
	if err != nil {
		c.Ui.Error(fmt.Sprintf(
			"Error initializing client: %s", err))
		return 1
	}

	if check {
		return c.checkStatus(client)
	}

	resp, err := client.Sys().Init(initRequest)
	if err != nil {
		c.Ui.Error(fmt.Sprintf(
			"Error initializing Vault: %s", err))
		return 1
	}

	for i, key := range resp.Keys {
		c.Ui.Output(fmt.Sprintf("Unseal Key %d: %s", i+1, key))
	}
	for i, key := range resp.RecoveryKeys {
		c.Ui.Output(fmt.Sprintf("Recovery Key %d: %s", i+1, key))
	}

	c.Ui.Output(fmt.Sprintf("Initial Root Token: %s", resp.RootToken))

	if initRequest.StoredShares < 1 {
		c.Ui.Output(fmt.Sprintf(
			"\n"+
				"Vault initialized with %d keys and a key threshold of %d. Please\n"+
				"securely distribute the above keys. When the Vault is re-sealed,\n"+
				"restarted, or stopped, you must provide at least %d of these keys\n"+
				"to unseal it again.\n\n"+
				"Vault does not store the master key. Without at least %d keys,\n"+
				"your Vault will remain permanently sealed.",
			initRequest.SecretShares,
			initRequest.SecretThreshold,
			initRequest.SecretThreshold,
			initRequest.SecretThreshold,
		))
	} else {
		c.Ui.Output(
			"\n" +
				"Vault initialized successfully.",
		)
	}
	if len(resp.RecoveryKeys) > 0 {
		c.Ui.Output(fmt.Sprintf(
			"\n"+
				"Recovery key initialized with %d keys and a key threshold of %d. Please\n"+
				"securely distribute the above keys.",
			initRequest.RecoveryShares,
			initRequest.RecoveryThreshold,
		))
	}

	return 0
}

func (c *InitCommand) checkStatus(client *api.Client) int {
	inited, err := client.Sys().InitStatus()
	switch {
	case err != nil:
		c.Ui.Error(fmt.Sprintf(
			"Error checking initialization status: %s", err))
		return 1
	case inited:
		c.Ui.Output("Vault has been initialized")
		return 0
	default:
		c.Ui.Output("Vault is not initialized")
		return 2
	}
}

func (c *InitCommand) Synopsis() string {
	return "Initialize a new Vault server"
}

func (c *InitCommand) Help() string {
	helpText := `
Usage: vault init [options]

  Initialize a new Vault server.

  This command connects to a Vault server and initializes it for the
  first time. This sets up the initial set of master keys and sets up the
  backend data store structure.

  This command can't be called on an already-initialized Vault.

General Options:
` + meta.GeneralOptionsUsage() + `
Init Options:

  -check                    Don't actually initialize, just check if Vault is
                            already initialized. A return code of 0 means Vault
                            is initialized; a return code of 2 means Vault is not
                            initialized; a return code of 1 means an error was
                            encountered.

  -key-shares=5             The number of key shares to split the master key
                            into.

  -key-threshold=3          The number of key shares required to reconstruct
                            the master key.

  -stored-shares=0          The number of unseal keys to store. This is not
                            normally available.

  -pgp-keys                 If provided, must be a comma-separated list of
                            files on disk containing binary- or base64-format
                            public PGP keys, or Keybase usernames specified as
                            "keybase:<username>". The number of given entries
                            must match 'key-shares'. The output unseal keys will
                            be encrypted and hex-encoded, in order, with the
                            given public keys.  If you want to use them with the
                            'vault unseal' command, you will need to hex decode
                            and decrypt; this will be the plaintext unseal key.

  -recovery-shares=5        The number of key shares to split the recovery key
                            into. This is not normally available.

  -recovery-threshold=3     The number of key shares required to reconstruct
                            the recovery key. This is not normally available.

  -recovery-pgp-keys        If provided, behaves like "pgp-keys" but for the
                            recovery key shares. This is not normally available.
`
	return strings.TrimSpace(helpText)
}
