package start

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/coreos/go-systemd/daemon"
	"github.com/golang/glog"
	"github.com/spf13/cobra"

	kerrors "k8s.io/kubernetes/pkg/api/errors"
	kcmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/util/fielderrors"

	configapilatest "github.com/openshift/origin/pkg/cmd/server/api/latest"
	"github.com/openshift/origin/pkg/cmd/server/api/validation"
	"github.com/openshift/origin/pkg/cmd/server/etcd/etcdserver"
)

const RecommendedStartEtcdServerName = "etcd"

type EtcdOptions struct {
	ConfigFile string
	Output     io.Writer
}

const etcdLong = `Start an etcd server for testing.

This command starts an etcd server based on the config for testing.  It is not 
Intended for production use.  Running

  $ %[1]s start %[2]s

will start the server listening for incoming requests. The server
will run in the foreground until you terminate the process.`

// NewCommandStartEtcdServer starts only the etcd server
func NewCommandStartEtcdServer(name, basename string, out io.Writer) (*cobra.Command, *EtcdOptions) {
	options := &EtcdOptions{Output: out}

	cmd := &cobra.Command{
		Use:   name,
		Short: "Launch etcd server",
		Long:  fmt.Sprintf(etcdLong, basename, name),
		Run: func(c *cobra.Command, args []string) {
			kcmdutil.CheckErr(options.Validate())

			startProfiler()

			if err := options.StartEtcdServer(); err != nil {
				if kerrors.IsInvalid(err) {
					if details := err.(*kerrors.StatusError).ErrStatus.Details; details != nil {
						fmt.Fprintf(c.Out(), "Invalid %s %s\n", details.Kind, details.Name)
						for _, cause := range details.Causes {
							fmt.Fprintf(c.Out(), "  %s: %s\n", cause.Field, cause.Message)
						}
						os.Exit(255)
					}
				}
				glog.Fatal(err)
			}
		},
	}

	flags := cmd.Flags()
	// This command only supports reading from config
	flags.StringVar(&options.ConfigFile, "config", "", "Location of the master configuration file to run from.")
	cmd.MarkFlagFilename("config", "yaml", "yml")
	cmd.MarkFlagRequired("config")

	return cmd, options
}

func (o *EtcdOptions) Validate() error {
	if len(o.ConfigFile) == 0 {
		return errors.New("--config is required for this command")
	}

	return nil
}

// StartEtcdServer calls RunEtcdServer and then waits forever
func (o *EtcdOptions) StartEtcdServer() error {
	if err := o.RunEtcdServer(); err != nil {
		return err
	}

	go daemon.SdNotify("READY=1")
	select {}
}

// RunEtcdServer takes the options and starts the etcd server
func (o *EtcdOptions) RunEtcdServer() error {
	masterConfig, err := configapilatest.ReadAndResolveMasterConfig(o.ConfigFile)
	if err != nil {
		return err
	}

	validationResults := validation.ValidateMasterConfig(masterConfig)
	if len(validationResults.Warnings) != 0 {
		for _, warning := range validationResults.Warnings {
			glog.Warningf("%v", warning)
		}
	}
	if len(validationResults.Errors) != 0 {
		return kerrors.NewInvalid("MasterConfig", o.ConfigFile, validationResults.Errors)
	}

	if masterConfig.EtcdConfig == nil {
		return kerrors.NewInvalid("MasterConfig.EtcConfig", o.ConfigFile, fielderrors.ValidationErrorList{fielderrors.NewFieldRequired("etcdConfig")})
	}

	etcdserver.RunEtcd(masterConfig.EtcdConfig)
	return nil
}
