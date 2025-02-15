package builder

import (
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	dockercmd "github.com/docker/docker/builder/command"
	"github.com/docker/docker/builder/parser"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/golang/glog"
	kapi "k8s.io/kubernetes/pkg/api"

	"github.com/openshift/source-to-image/pkg/tar"
	"github.com/openshift/source-to-image/pkg/util"

	"github.com/openshift/origin/pkg/build/api"
	"github.com/openshift/origin/pkg/build/builder/cmd/dockercfg"
	"github.com/openshift/origin/pkg/build/controller/strategy"
	"github.com/openshift/origin/pkg/client"
	"github.com/openshift/origin/pkg/generate/git"
	imageapi "github.com/openshift/origin/pkg/image/api"
	"github.com/openshift/origin/pkg/util/docker/dockerfile"
)

// defaultDockerfilePath is the default path of the Dockerfile
const defaultDockerfilePath = "Dockerfile"

// DockerBuilder builds Docker images given a git repository URL
type DockerBuilder struct {
	dockerClient DockerClient
	gitClient    GitClient
	tar          tar.Tar
	build        *api.Build
	urlTimeout   time.Duration
	client       client.BuildInterface
}

// NewDockerBuilder creates a new instance of DockerBuilder
func NewDockerBuilder(dockerClient DockerClient, buildsClient client.BuildInterface, build *api.Build, gitClient GitClient) *DockerBuilder {
	return &DockerBuilder{
		dockerClient: dockerClient,
		build:        build,
		gitClient:    gitClient,
		tar:          tar.New(),
		urlTimeout:   urlCheckTimeout,
		client:       buildsClient,
	}
}

// Build executes a Docker build
func (d *DockerBuilder) Build() error {
	var push bool

	buildDir, err := ioutil.TempDir("", "docker-build")
	if err != nil {
		return err
	}
	sourceInfo, err := fetchSource(d.dockerClient, buildDir, d.build, d.urlTimeout, os.Stdin, d.gitClient)
	if err != nil {
		return err
	}
	if sourceInfo != nil {
		updateBuildRevision(d.client, d.build, sourceInfo)
	}
	if err := d.addBuildParameters(buildDir); err != nil {
		return err
	}

	glog.V(4).Infof("Starting Docker build from build config %s ...", d.build.Name)
	// if there is no output target, set one up so the docker build logic
	// (which requires a tag) will still work, but we won't push it at the end.
	if d.build.Spec.Output.To == nil || len(d.build.Spec.Output.To.Name) == 0 {
		d.build.Status.OutputDockerImageReference = d.build.Name
	} else {
		push = true
	}

	if err := d.dockerBuild(buildDir, d.build.Spec.Source.Secrets); err != nil {
		return err
	}

	defer removeImage(d.dockerClient, d.build.Status.OutputDockerImageReference)

	if push {
		// Get the Docker push authentication
		pushAuthConfig, authPresent := dockercfg.NewHelper().GetDockerAuth(
			d.build.Status.OutputDockerImageReference,
			dockercfg.PushAuthType,
		)
		if authPresent {
			glog.V(4).Infof("Authenticating Docker push with user %q", pushAuthConfig.Username)
		}
		glog.Infof("Pushing image %s ...", d.build.Status.OutputDockerImageReference)
		if err := pushImage(d.dockerClient, d.build.Status.OutputDockerImageReference, pushAuthConfig); err != nil {
			return fmt.Errorf("Failed to push image: %v", err)
		}
		glog.Infof("Push successful")
	}
	return nil
}

// copySecrets copies all files from the directory where the secret is
// mounted in the builder pod to a directory where the is the Dockerfile, so
// users can ADD or COPY the files inside their Dockerfile.
func (d *DockerBuilder) copySecrets(secrets []api.SecretBuildSource, buildDir string) error {
	for _, s := range secrets {
		dstDir := filepath.Join(buildDir, s.DestinationDir)
		if err := os.MkdirAll(dstDir, 0777); err != nil {
			return err
		}
		srcDir := filepath.Join(strategy.SecretBuildSourceBaseMountPath, s.Secret.Name)
		glog.Infof("Copying files from the build secret %q to %q", s.Secret.Name, filepath.Clean(s.DestinationDir))
		out, err := exec.Command("cp", "-vrf", srcDir+"/.", dstDir+"/").Output()
		if err != nil {
			glog.Infof("Secret %q failed to copy: %q", s.Secret.Name, string(out))
			return err
		}
		// See what is copied where when debugging.
		glog.V(5).Infof(string(out))
	}
	return nil
}

// addBuildParameters checks if a Image is set to replace the default base image.
// If that's the case then change the Dockerfile to make the build with the given image.
// Also append the environment variables and labels in the Dockerfile.
func (d *DockerBuilder) addBuildParameters(dir string) error {
	var contextDirPath string
	if d.build.Spec.Strategy.DockerStrategy != nil && len(d.build.Spec.Source.ContextDir) > 0 {
		contextDirPath = filepath.Join(dir, d.build.Spec.Source.ContextDir)
	} else {
		contextDirPath = dir
	}

	var dockerfilePath string
	if d.build.Spec.Strategy.DockerStrategy != nil && len(d.build.Spec.Strategy.DockerStrategy.DockerfilePath) > 0 {
		dockerfilePath = filepath.Join(contextDirPath, d.build.Spec.Strategy.DockerStrategy.DockerfilePath)
	} else {
		dockerfilePath = filepath.Join(contextDirPath, defaultDockerfilePath)
	}

	f, err := os.Open(dockerfilePath)
	if err != nil {
		return err
	}

	// Parse the Dockerfile.
	node, err := parser.Parse(f)
	if err != nil {
		return err
	}

	// Update base image if build strategy specifies the From field.
	if d.build.Spec.Strategy.DockerStrategy.From != nil && d.build.Spec.Strategy.DockerStrategy.From.Kind == "DockerImage" {
		// Reduce the name to a minimal canonical form for the daemon
		name := d.build.Spec.Strategy.DockerStrategy.From.Name
		if ref, err := imageapi.ParseDockerImageReference(name); err == nil {
			name = ref.DaemonMinimal().String()
		}
		err := replaceLastFrom(node, name)
		if err != nil {
			return err
		}
	}

	// Append build info as environment variables.
	err = appendEnv(node, d.buildInfo())
	if err != nil {
		return err
	}

	// Append build labels.
	err = appendLabel(node, d.buildLabels(dir))
	if err != nil {
		return err
	}

	// Insert environment variables defined in the build strategy.
	err = insertEnvAfterFrom(node, d.build.Spec.Strategy.DockerStrategy.Env)
	if err != nil {
		return err
	}

	instructions := dockerfile.ParseTreeToDockerfile(node)

	// Overwrite the Dockerfile.
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return ioutil.WriteFile(dockerfilePath, instructions, fi.Mode())
}

// buildInfo converts the buildInfo output to a format that appendEnv can
// consume.
func (d *DockerBuilder) buildInfo() []dockerfile.KeyValue {
	bi := buildInfo(d.build)
	kv := make([]dockerfile.KeyValue, len(bi))
	for i, item := range bi {
		kv[i] = dockerfile.KeyValue{Key: item.Key, Value: item.Value}
	}
	return kv
}

// buildLabels returns a slice of KeyValue pairs in a format that appendEnv can
// consume.
func (d *DockerBuilder) buildLabels(dir string) []dockerfile.KeyValue {
	labels := map[string]string{}
	// TODO: allow source info to be overriden by build
	sourceInfo := &git.SourceInfo{}
	if d.build.Spec.Source.Git != nil {
		var errors []error
		sourceInfo, errors = d.gitClient.GetInfo(dir)
		if len(errors) > 0 {
			for _, e := range errors {
				glog.Warningf("Error getting git info: %v", e.Error())
			}
		}
	}
	if len(d.build.Spec.Source.ContextDir) > 0 {
		sourceInfo.ContextDir = d.build.Spec.Source.ContextDir
	}
	labels = util.GenerateLabelsFromSourceInfo(labels, &sourceInfo.SourceInfo, api.DefaultDockerLabelNamespace)
	kv := make([]dockerfile.KeyValue, 0, len(labels))
	for k, v := range labels {
		kv = append(kv, dockerfile.KeyValue{Key: k, Value: v})
	}
	return kv
}

// setupPullSecret provides a Docker authentication configuration when the
// PullSecret is specified.
func (d *DockerBuilder) setupPullSecret() (*docker.AuthConfigurations, error) {
	if len(os.Getenv(dockercfg.PullAuthType)) == 0 {
		return nil, nil
	}
	r, err := os.Open(os.Getenv(dockercfg.PullAuthType))
	if err != nil {
		return nil, fmt.Errorf("'%s': %s", os.Getenv(dockercfg.PullAuthType), err)
	}
	return docker.NewAuthConfigurations(r)
}

// dockerBuild performs a docker build on the source that has been retrieved
func (d *DockerBuilder) dockerBuild(dir string, secrets []api.SecretBuildSource) error {
	var noCache bool
	var forcePull bool
	dockerfilePath := defaultDockerfilePath
	if d.build.Spec.Strategy.DockerStrategy != nil {
		if d.build.Spec.Source.ContextDir != "" {
			dir = filepath.Join(dir, d.build.Spec.Source.ContextDir)
		}
		if d.build.Spec.Strategy.DockerStrategy.DockerfilePath != "" {
			dockerfilePath = d.build.Spec.Strategy.DockerStrategy.DockerfilePath
		}
		noCache = d.build.Spec.Strategy.DockerStrategy.NoCache
		forcePull = d.build.Spec.Strategy.DockerStrategy.ForcePull
	}
	auth, err := d.setupPullSecret()
	if err != nil {
		return err
	}
	if err := d.copySecrets(secrets, dir); err != nil {
		return err
	}
	return buildImage(d.dockerClient, dir, dockerfilePath, noCache, d.build.Status.OutputDockerImageReference, d.tar, auth, forcePull)
}

// replaceLastFrom changes the last FROM instruction of node to point to the
// base image image.
func replaceLastFrom(node *parser.Node, image string) error {
	if node == nil {
		return nil
	}
	for i := len(node.Children) - 1; i >= 0; i-- {
		child := node.Children[i]
		if child != nil && child.Value == dockercmd.From {
			from, err := dockerfile.From(image)
			if err != nil {
				return err
			}
			fromTree, err := parser.Parse(strings.NewReader(from))
			if err != nil {
				return err
			}
			node.Children[i] = fromTree.Children[0]
			return nil
		}
	}
	return nil
}

// appendEnv appends an ENV Dockerfile instruction as the last child of node
// with keys and values from m.
func appendEnv(node *parser.Node, m []dockerfile.KeyValue) error {
	return appendKeyValueInstruction(dockerfile.Env, node, m)
}

// appendLabel appends a LABEL Dockerfile instruction as the last child of node
// with keys and values from m.
func appendLabel(node *parser.Node, m []dockerfile.KeyValue) error {
	if len(m) == 0 {
		return nil
	}
	return appendKeyValueInstruction(dockerfile.Label, node, m)
}

// appendKeyValueInstruction is a primitive used to avoid code duplication.
// Callers should use a derivative of this such as appendEnv or appendLabel.
// appendKeyValueInstruction appends a Dockerfile instruction with key-value
// syntax created by f as the last child of node with keys and values from m.
func appendKeyValueInstruction(f func([]dockerfile.KeyValue) (string, error), node *parser.Node, m []dockerfile.KeyValue) error {
	if node == nil {
		return nil
	}
	instruction, err := f(m)
	if err != nil {
		return err
	}
	return dockerfile.InsertInstructions(node, len(node.Children), instruction)
}

// insertEnvAfterFrom inserts an ENV instruction with the environment variables
// from env after every FROM instruction in node.
func insertEnvAfterFrom(node *parser.Node, env []kapi.EnvVar) error {
	if node == nil || len(env) == 0 {
		return nil
	}

	// Build ENV instruction.
	var m []dockerfile.KeyValue
	for _, e := range env {
		m = append(m, dockerfile.KeyValue{Key: e.Name, Value: e.Value})
	}
	buildEnv, err := dockerfile.Env(m)
	if err != nil {
		return err
	}

	// Insert the buildEnv after every FROM instruction.
	// We iterate in reverse order, otherwise indices would have to be
	// recomputed after each step, because we're changing node in-place.
	indices := dockerfile.FindAll(node, dockercmd.From)
	for i := len(indices) - 1; i >= 0; i-- {
		err := dockerfile.InsertInstructions(node, indices[i]+1, buildEnv)
		if err != nil {
			return err
		}
	}

	return nil
}
