package command

// This file contains all the Backend-related function calls on Meta,
// exported and private.

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/errwrap"
	"github.com/hashicorp/go-multierror"
	"github.com/hashicorp/terraform/backend"
	backendlegacy "github.com/hashicorp/terraform/backend/legacy"
	backendlocal "github.com/hashicorp/terraform/builtin/backends/local"
	"github.com/hashicorp/terraform/config"
	"github.com/hashicorp/terraform/state"
	"github.com/hashicorp/terraform/terraform"
	"github.com/mitchellh/mapstructure"
)

// BackendOpts are the options used to initialize a backend.Backend.
type BackendOpts struct {
	// ConfigPath is a path to a file or directory containing the backend
	// configuration. If "-backend-config" is set and processed on Meta,
	// that will take priority.
	ConfigPath string

	// ForceLocal will force a purely local backend, including state.
	// You probably don't want to set this.
	ForceLocal bool
}

// Backend initializes and returns the backend for this CLI session.
//
// The backend is used to perform the actual Terraform operations. This
// abstraction enables easily sliding in new Terraform behavior such as
// remote state storage, remote operations, etc. while allowing the CLI
// to remain mostly identical.
//
// This will initialize a new backend for each call, which can carry some
// overhead with it. Please reuse the returned value for optimal behavior.
func (m *Meta) Backend(opts *BackendOpts) (backend.Enhanced, error) {
	// If no opts are set, then initialize
	if opts == nil {
		opts = &BackendOpts{}
	}

	// Setup the local state paths
	statePath := m.statePath
	stateOutPath := m.stateOutPath
	backupPath := m.backupPath
	if statePath == "" {
		statePath = DefaultStateFilename
	}
	if stateOutPath == "" {
		stateOutPath = statePath
	}
	if backupPath == "" {
		backupPath = stateOutPath + DefaultBackupExtension
	}
	if backupPath == "-" {
		// The local backend expects an empty string for not taking backups.
		backupPath = ""
	}

	// Initialize a backend from the config unless we're forcing a purely
	// local operation.
	var b backend.Backend
	if !opts.ForceLocal {
		// Get the local backend configuration.
		config, err := m.backendConfig(opts)
		if err != nil {
			return nil, fmt.Errorf("Error loading backend config: %s", err)
		}

		// Get the path to where we store a local cache of backend configuration
		// if we're using a remote backend. This may not yet exist which means
		// we haven't used a non-local backend before. That is okay.
		dataStatePath := filepath.Join(m.DataDir(), DefaultStateFilename)
		dataStateMgr := &state.LocalState{Path: dataStatePath}
		if err := dataStateMgr.RefreshState(); err != nil {
			return nil, fmt.Errorf("Error loading state: %s", err)
		}

		// Get the final backend configuration to use. This will handle any
		// conflicts (legacy remote state, new config, config changes, etc.)
		// and only return the final configuration to use.
		b, err = m.backendFromConfig(config, dataStateMgr)
		if err != nil {
			return nil, err
		}
	}

	log.Printf("[INFO] command: backend initialized: %T", b)

	// If the result of loading the backend is an enhanced backend,
	// then return that as-is. This works even if b == nil (it will be !ok).
	if enhanced, ok := b.(backend.Enhanced); ok {
		return enhanced, nil
	}

	// We either have a non-enhanced backend or no backend configured at
	// all. In either case, we use local as our enhanced backend and the
	// non-enhanced (if any) as the state backend.

	log.Printf("[INFO] command: backend %T is not enhanced, wrapping in local", b)

	// Build the local backend
	return &backendlocal.Local{
		CLI:             m.Ui,
		CLIColor:        m.Colorize(),
		StatePath:       statePath,
		StateOutPath:    stateOutPath,
		StateBackupPath: backupPath,
		ContextOpts:     m.contextOpts(),
		Input:           m.Input(),
		Validation:      true,
		Backend:         b,
	}, nil
}

// Operation initializes a new backend.Operation struct.
//
// This prepares the operation. After calling this, the caller is expected
// to modify fields of the operation such as Sequence to specify what will
// be called.
func (m *Meta) Operation() *backend.Operation {
	return &backend.Operation{
		Targets: m.targets,
		UIIn:    m.UIInput(),
	}
}

// backendConfig returns the local configuration for the backend
func (m *Meta) backendConfig(opts *BackendOpts) (*config.Backend, error) {
	// If no explicit path was given then it is okay for there to be
	// no backend configuration found.
	emptyOk := opts.ConfigPath == "" && m.backendConfigPath == ""

	// Determine the path to the configuration. If the "-backend-config"
	// flag was set, that always takes priority.
	path := opts.ConfigPath
	if m.backendConfigPath != "" {
		path = m.backendConfigPath
	}

	// If we had no path set, it is an error. We can't initialize unset
	if path == "" {
		path = "."
	}

	// Expand the path
	if !filepath.IsAbs(path) {
		var err error
		path, err = filepath.Abs(path)
		if err != nil {
			return nil, fmt.Errorf(
				"Error expanding path to backend config %q: %s", path, err)
		}
	}

	log.Printf("[DEBUG] command: loading backend config file: %s", path)

	// We first need to determine if we're loading a file or a directory.
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) && emptyOk {
			log.Printf(
				"[INFO] command: backend config not found, returning nil: %s",
				path)
			return nil, nil
		}

		return nil, err
	}

	var f func(string) (*config.Config, error) = config.LoadFile
	if fi.IsDir() {
		f = config.LoadDir
	}

	// Load the configuration
	c, err := f(path)
	if err != nil {
		// Check for the error where we have no config files and return nil
		// as the configuration type.
		if emptyOk && errwrap.ContainsType(err, new(config.ErrNoConfigsFound)) {
			log.Printf(
				"[INFO] command: backend config not found, returning nil: %s",
				path)
			return nil, nil
		}

		return nil, err
	}

	// If there is no Terraform configuration block, no backend config
	if c.Terraform == nil {
		return nil, nil
	}

	// Return the configuration which may or may not be set
	return c.Terraform.Backend, nil
}

// backendFromConfig returns the initialized (not configured) backend
// directly from the config/state..
//
// This function handles any edge cases around backend config loading. For
// example: legacy remote state, new config changes, backend type changes,
// etc.
//
// This function may query the user for input unless input is disabled, in
// which case this function will error.
func (m *Meta) backendFromConfig(
	c *config.Backend, sMgr state.State) (backend.Backend, error) {
	// Load the state, it must be non-nil for the tests below but can be empty
	s := sMgr.State()
	if s == nil {
		log.Printf("[DEBUG] command: no data state file found for backend config")
		s = terraform.NewState()
	}

	switch {
	// No configuration set at all. Pure local state.
	case c == nil && s.Remote.Empty() && s.Backend.Empty():
		return nil, nil

	// We're unsetting a backend (moving from backend => local)
	case c == nil && s.Remote.Empty() && !s.Backend.Empty():
		panic("unhandled")

	// We have a legacy remote state configuration but no new backend config
	case c == nil && !s.Remote.Empty() && s.Backend.Empty():
		return m.backend_c_R_s(c, sMgr)

	// We have a legacy remote state configuration simultaneously with a
	// saved backend configuration while at the same time disabling backend
	// configuration.
	//
	// This is a naturally impossible case: Terraform will never put you
	// in this state, though it is theoretically possible through manual edits
	case c == nil && !s.Remote.Empty() && !s.Backend.Empty():
		panic("unhandled")

	// Configuring a backend for the first time.
	case c != nil && s.Remote.Empty() && s.Backend.Empty():
		return m.backend_C_r_s(c, sMgr)

	// Potentially changing a backend configuration
	case c != nil && s.Remote.Empty() && !s.Backend.Empty():
		// If our configuration is the same, then we're just initializing
		// a previously configured remote backend.
		if !s.Backend.Empty() && s.Backend.Hash == c.Hash() {
			return m.backend_C_r_S_unchanged(c, sMgr)
		}

		panic("unhandled")

	// Configuring a backend for the first time while having legacy
	// remote state. This is very possible if a Terraform user configures
	// a backend prior to ever running Terraform on an old state.
	case c != nil && !s.Remote.Empty() && s.Backend.Empty():
		panic("unhandled")

	// Configuring a backend with both a legacy remote state set
	// and a pre-existing backend saved.
	case c != nil && !s.Remote.Empty() && !s.Backend.Empty():
		// If the hashes are the same, we have a legacy remote state with
		// an unchanged stored backend state.
		if s.Backend.Hash == c.Hash() {
			panic("unhandled")
		}

		// We have change in all three
		panic("unhandled")
	default:
		// This should be impossible since all state possibilties are
		// tested above, but we need a default case anyways and we should
		// protect against the scenario where a case is somehow removed.
		return nil, fmt.Errorf(
			"Unhandled backend configuration state. This is a bug. Please\n"+
				"report this error with the following information.\n\n"+
				"Config Nil: %v\n"+
				"Saved Backend Empty: %v\n"+
				"Legacy Remote Empty: %v\n",
			c == nil, s.Backend.Empty(), s.Remote.Empty())
	}
}

//-------------------------------------------------------------------
// Backend Config Scenarios
//
// The functions below cover handling all the various scenarios that
// can exist when loading a backend. They are named in the format of
// "backend_C_R_S" where C, R, S may be upper or lowercase. Lowercase
// means it is false, uppercase means it is true. The full set of eight
// possible cases is handled.
//
// The fields are:
//
//   * C - Backend configuration is set and changed in TF files
//   * R - Legacy remote state is set
//   * S - Backend configuration is set in the state
//
//-------------------------------------------------------------------

func (m *Meta) backend_c_R_s(
	c *config.Backend, sMgr state.State) (backend.Backend, error) {
	s := sMgr.State()

	// Warn the user
	m.Ui.Warn(strings.TrimSpace(warnBackendLegacy))

	// We need to convert the config to map[string]interface{} since that
	// is what the backends expect.
	var configMap map[string]interface{}
	if err := mapstructure.Decode(s.Remote.Config, &configMap); err != nil {
		return nil, fmt.Errorf("Error configuring remote state: %s", err)
	}

	// Create the config
	rawC, err := config.NewRawConfig(configMap)
	if err != nil {
		return nil, fmt.Errorf("Error configuring remote state: %s", err)
	}
	config := terraform.NewResourceConfig(rawC)

	// Initialize the legacy remote backend
	b := &backendlegacy.Backend{Type: s.Remote.Type}

	// Configure
	if err := b.Configure(config); err != nil {
		return nil, fmt.Errorf(errBackendLegacyConfig, err)
	}

	return b, nil
}

// Configuring a backend for the first time.
func (m *Meta) backend_C_r_s(
	c *config.Backend, sMgr state.State) (backend.Backend, error) {
	// Create the config.
	config := terraform.NewResourceConfig(c.RawConfig)

	// Get the backend
	f, ok := Backends[c.Type]
	if !ok {
		return nil, fmt.Errorf(strings.TrimSpace(errBackendNewUnknown), c.Type)
	}
	b := f()

	// TODO: input

	// Validate
	warns, errs := b.Validate(config)
	if len(errs) > 0 {
		return nil, fmt.Errorf(
			"Error configuring the backend %q: %s",
			c.Type, multierror.Append(nil, errs...))
	}
	if len(warns) > 0 {
		// TODO: warnings are currently ignored
	}

	// Configure
	if err := b.Configure(config); err != nil {
		return nil, fmt.Errorf(errBackendNewConfig, c.Type, err)
	}

	// Grab a purely local backend to get the local state if it exists
	localB, err := m.Backend(&BackendOpts{ForceLocal: true})
	if err != nil {
		return nil, fmt.Errorf(errBackendLocalRead, err)
	}
	localState, err := localB.State()
	if err != nil {
		return nil, fmt.Errorf(errBackendLocalRead, err)
	}
	if err := localState.RefreshState(); err != nil {
		return nil, fmt.Errorf(errBackendLocalRead, err)
	}

	// If the local state is not empty, we need to potentially do a
	// state migration to the new backend (with user permission).
	if localS := localState.State(); !localS.Empty() {
		backendState, err := b.State()
		if err != nil {
			return nil, fmt.Errorf(errBackendRemoteRead, err)
		}
		if err := backendState.RefreshState(); err != nil {
			return nil, fmt.Errorf(errBackendRemoteRead, err)
		}

		// Perform the migration
		err = m.backendMigrateState(&backendMigrateOpts{
			OneType: "local",
			TwoType: c.Type,
			One:     localState,
			Two:     backendState,
		})
		if err != nil {
			return nil, err
		}

		// We always delete the local state
		if err := localState.WriteState(nil); err != nil {
			return nil, fmt.Errorf(errBackendMigrateLocalDelete, err)
		}
		if err := localState.PersistState(); err != nil {
			return nil, fmt.Errorf(errBackendMigrateLocalDelete, err)
		}
	}

	// Store the metadata in our saved state location
	s := sMgr.State()
	if s == nil {
		s = terraform.NewState()
	}
	s.Backend = &terraform.BackendState{
		Type:   c.Type,
		Config: config.Raw,
		Hash:   c.Hash(),
	}
	if err := sMgr.WriteState(s); err != nil {
		return nil, fmt.Errorf(errBackendWriteSaved, err)
	}
	if err := sMgr.PersistState(); err != nil {
		return nil, fmt.Errorf(errBackendWriteSaved, err)
	}

	// Return the backend
	return b, nil
}

// Initiailizing an unchanged saved backend
func (m *Meta) backend_C_r_S_unchanged(
	c *config.Backend, sMgr state.State) (backend.Backend, error) {
	s := sMgr.State()

	// Create the config. We do this from the backend state since this
	// has the complete configuration data whereas the config itself
	// may require input.
	rawC, err := config.NewRawConfig(s.Backend.Config)
	if err != nil {
		return nil, fmt.Errorf("Error configuring backend: %s", err)
	}
	config := terraform.NewResourceConfig(rawC)

	// Get the backend
	f, ok := Backends[s.Backend.Type]
	if !ok {
		return nil, fmt.Errorf(strings.TrimSpace(errBackendSavedUnknown), s.Backend.Type)
	}
	b := f()

	// Configure
	if err := b.Configure(config); err != nil {
		return nil, fmt.Errorf(errBackendSavedConfig, s.Backend.Type, err)
	}

	return b, nil
}

// Backends is the list of available backends. This is currently a hardcoded
// list that can't be modified without recompiling Terraform. This is done
// because the API for backends uses complex structures and supporting that
// over the plugin system is currently prohibitively difficult. For those
// wanting to implement a custom backend, recompilation should not be a
// high barrier.
var Backends map[string]func() backend.Backend

func init() {
	// Our hardcoded backends
	Backends = map[string]func() backend.Backend{
		"local": func() backend.Backend { return &backendlocal.Local{} },
	}

	// Add the legacy remote backends that haven't yet been convertd to
	// the new backend API.
	backendlegacy.Init(Backends)
}

const errBackendLegacyConfig = `
One or more errors occurred while configuring the legacy remote state.
If fixing these errors requires changing your remote state configuration,
you must switch your configuration to the new remote backend configuration.
You can learn more about remote backends at the URL below:

TODO: URL

The error(s) configuring the legacy remote state:

%s
`

const errBackendLocalRead = `
Error reading local state: %s

Terraform is trying to read your local state to determine if there is
state to migrate to your newly configured backend. Terraform can't continue
without this check because that would risk losing state. Please resolve the
error above and try again.
`

const errBackendMigrateLocalDelete = `
Error deleting local state after migration: %s

Your local state is deleted after successfully migrating it to the newly
configured backend. As part of the deletion process, a backup is made at
the standard backup path unless explicitly asked not to. To cleanly operate
with a backend, we must delete the local state file. Please resolve the
issue above and retry the command.
`

const errBackendMigrateNew = `
Error migrating local state to backend: %s

Your local state remains intact and unmodified. Please resolve the error
above and try again.
`

const errBackendNewConfig = `
Error configuring the backend %q: %s

Please update the configuration in your Terraform files to fix this error
then run this command again.
`

const errBackendNewUnknown = `
The backend %q could not be found.

This is the backend specified in your Terraform configuration file.
This error could be a simple typo in your configuration, but it can also
be caused by using a Terraform version that doesn't support the specified
backend type. Please check your configuration and your Terraform version.

If you'd like to run Terraform and store state locally, you can fix this
error by removing the backend configuration from your configuration.
`

const errBackendRemoteRead = `
Error reading backend state: %s

Terraform is trying to read the state from your configured backend to
determien if there is any migration steps necessary. Terraform can't continue
without this check because that would risk losing state. Please resolve the
error above and try again.
`

const errBackendSavedConfig = `
Error configuring the backend %q: %s

Please update the configuration in your Terraform files to fix this error.
If you'd like to update the configuration interactively without storing
the values in your configuration, run "terraform init".
`

const errBackendSavedUnknown = `
The backend %q could not be found.

This is the backend that this Terraform environment is configured to use
both in your configuration and saved locally as your last-used backend.
If it isn't found, it could mean an alternate version of Terraform was
used with this configuration. Please use the proper version of Terraform that
contains support for this backend.

If you'd like to force remove this backend, you must update your configuration
to not use the backend and run "terraform init" (or any other command) again.
`

const errBackendWriteSaved = `
Error saving the backend configuration: %s

Terraform saves the complete backend configuration in a local file for
configuring the backend on future operations. This cannot be disabled. Errors
are usually due to simple file permission errors. Please look at the error
above, resolve it, and try again.
`

const warnBackendLegacy = `
Deprecation warning: This environment is configured to use legacy remote state.
Remote state changed significantly in Terraform 0.9. Please update your remote
state configuration to use the new 'backend' settings. For now, Terraform
will continue to use your existing settings. Legacy remote state support
will be removed in Terraform 0.11.
`
