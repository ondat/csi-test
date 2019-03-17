/*
Copyright 2017 Luis Pabón luis@portworx.com

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sanity

import (
	"context"
	"crypto/rand"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kubernetes-csi/csi-test/utils"
	yaml "gopkg.in/yaml.v2"

	"google.golang.org/grpc"

	. "github.com/onsi/ginkgo"
	"github.com/onsi/ginkgo/reporters"
	. "github.com/onsi/gomega"
)

// CSISecrets consists of secrets used in CSI credentials.
type CSISecrets struct {
	CreateVolumeSecret              map[string]string `yaml:"CreateVolumeSecret"`
	DeleteVolumeSecret              map[string]string `yaml:"DeleteVolumeSecret"`
	ControllerPublishVolumeSecret   map[string]string `yaml:"ControllerPublishVolumeSecret"`
	ControllerUnpublishVolumeSecret map[string]string `yaml:"ControllerUnpublishVolumeSecret"`
	NodeStageVolumeSecret           map[string]string `yaml:"NodeStageVolumeSecret"`
	NodePublishVolumeSecret         map[string]string `yaml:"NodePublishVolumeSecret"`
	CreateSnapshotSecret            map[string]string `yaml:"CreateSnapshotSecret"`
	DeleteSnapshotSecret            map[string]string `yaml:"DeleteSnapshotSecret"`
}

// Config provides the configuration for the sanity tests. It
// needs to be initialized by the user of the sanity package.
type Config struct {
	TargetPath        string
	StagingPath       string
	Address           string
	ControllerAddress string
	SecretsFile       string

	TestVolumeSize            int64
	TestVolumeParametersFile  string
	TestVolumeParameters      map[string]string
	TestNodeVolumeAttachLimit bool

	JUnitFile string

	// Callback functions to customize the creation of target and staging
	// directories. Returns the new paths for mount and staging.
	// If not defined, directories are created in the default way at TargetPath
	// and StagingPath on the host.
	CreateTargetDir  func(path string) (string, error)
	CreateStagingDir func(path string) (string, error)

	// Callback functions to customize the removal of the target and staging
	// directories.
	// If not defined, directories are removed in the default way at TargetPath
	// and StagingPath on the host.
	RemoveTargetPath  func(path string) error
	RemoveStagingPath func(path string) error

	// Commands to be executed for customized creation of the target and staging
	// paths. This command must be available on the host where sanity runs. The
	// stdout of the commands are the paths for mount and staging.
	CreateTargetPathCmd  string
	CreateStagingPathCmd string
	// Timeout for the executed commands for path creation.
	CreatePathCmdTimeout int

	// Commands to be executed for customized removal of the target and staging
	// paths. Thie command must be available on the host where sanity runs.
	RemoveTargetPathCmd  string
	RemoveStagingPathCmd string
	// Timeout for the executed commands for path removal.
	RemovePathCmdTimeout int
}

// SanityContext holds the variables that each test can depend on. It
// gets initialized before each test block runs.
type SanityContext struct {
	Config         *Config
	Conn           *grpc.ClientConn
	ControllerConn *grpc.ClientConn
	Secrets        *CSISecrets

	connAddress           string
	controllerConnAddress string

	// Target and staging paths derived from the sanity config.
	targetPath  string
	stagingPath string
}

// Test will test the CSI driver at the specified address by
// setting up a Ginkgo suite and running it.
func Test(t *testing.T, reqConfig *Config) {
	path := reqConfig.TestVolumeParametersFile
	if len(path) != 0 {
		yamlFile, err := ioutil.ReadFile(path)
		if err != nil {
			panic(fmt.Sprintf("failed to read file %q: %v", path, err))
		}
		err = yaml.Unmarshal(yamlFile, &reqConfig.TestVolumeParameters)
		if err != nil {
			panic(fmt.Sprintf("error unmarshaling yaml: %v", err))
		}
	}

	sc := &SanityContext{
		Config: reqConfig,
	}

	registerTestsInGinkgo(sc)
	RegisterFailHandler(Fail)

	var specReporters []Reporter
	if reqConfig.JUnitFile != "" {
		junitReporter := reporters.NewJUnitReporter(reqConfig.JUnitFile)
		specReporters = append(specReporters, junitReporter)
	}
	RunSpecsWithDefaultAndCustomReporters(t, "CSI Driver Test Suite", specReporters)
	sc.Conn.Close()
}

func GinkgoTest(reqConfig *Config) {
	sc := &SanityContext{
		Config: reqConfig,
	}

	registerTestsInGinkgo(sc)
}

func (sc *SanityContext) setup() {
	var err error

	if len(sc.Config.SecretsFile) > 0 {
		sc.Secrets, err = loadSecrets(sc.Config.SecretsFile)
		Expect(err).NotTo(HaveOccurred())
	} else {
		sc.Secrets = &CSISecrets{}
	}

	// It is possible that a test sets sc.Config.Address
	// dynamically (and differently!) in a BeforeEach, so only
	// reuse the connection if the address is still the same.
	if sc.Conn == nil || sc.connAddress != sc.Config.Address {
		if sc.Conn != nil {
			sc.Conn.Close()
		}
		By("connecting to CSI driver")
		sc.Conn, err = utils.Connect(sc.Config.Address)
		Expect(err).NotTo(HaveOccurred())
		sc.connAddress = sc.Config.Address
	} else {
		By(fmt.Sprintf("reusing connection to CSI driver at %s", sc.connAddress))
	}

	if sc.ControllerConn == nil || sc.controllerConnAddress != sc.Config.ControllerAddress {
		// If controller address is empty, use the common connection.
		if sc.Config.ControllerAddress == "" {
			sc.ControllerConn = sc.Conn
			sc.controllerConnAddress = sc.Config.Address
		} else {
			sc.ControllerConn, err = utils.Connect(sc.Config.ControllerAddress)
			Expect(err).NotTo(HaveOccurred())
			sc.controllerConnAddress = sc.Config.ControllerAddress
		}
	} else {
		By(fmt.Sprintf("reusing connection to CSI driver controller at %s", sc.controllerConnAddress))
	}

	By("creating mount and staging directories")

	// If callback function for creating target dir is specified, use it.
	targetPath, err := createMountTargetLocation(sc.Config.TargetPath, sc.Config.CreateTargetPathCmd, sc.Config.CreateTargetDir, sc.Config.CreatePathCmdTimeout)
	Expect(err).NotTo(HaveOccurred(), "failed to create target directory %s", targetPath)
	sc.targetPath = targetPath

	// If callback function for creating staging dir is specified, use it.
	stagingPath, err := createMountTargetLocation(sc.Config.StagingPath, sc.Config.CreateStagingPathCmd, sc.Config.CreateStagingDir, sc.Config.CreatePathCmdTimeout)
	Expect(err).NotTo(HaveOccurred(), "failed to create staging directory %s", stagingPath)
	sc.stagingPath = stagingPath
}

func (sc *SanityContext) teardown() {
	// Delete the created paths if any.
	removeMountTargetLocation(sc.targetPath, sc.Config.RemoveTargetPathCmd, sc.Config.RemoveTargetPath, sc.Config.RemovePathCmdTimeout)
	removeMountTargetLocation(sc.stagingPath, sc.Config.RemoveStagingPathCmd, sc.Config.RemoveStagingPath, sc.Config.RemovePathCmdTimeout)

	// We intentionally do not close the connection to the CSI
	// driver here because the large amount of connection attempts
	// caused test failures
	// (https://github.com/kubernetes-csi/csi-test/issues/101). We
	// could fix this with retries
	// (https://github.com/kubernetes-csi/csi-test/pull/97) but
	// that requires more discussion, so instead we just connect
	// once per process instead of once per test case. This was
	// also said to be faster
	// (https://github.com/kubernetes-csi/csi-test/pull/98).
}

// createMountTargetLocation takes a target path parameter and creates the
// target path using a custom command, custom function or falls back to the
// default using mkdir and returns the new target path.
func createMountTargetLocation(targetPath string, createPathCmd string, customCreateDir func(string) (string, error), timeout int) (string, error) {

	// Return the target path if empty.
	if targetPath == "" {
		return targetPath, nil
	}

	var newTargetPath string

	if createPathCmd != "" {
		// Create the target path using the create path command.
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, createPathCmd, targetPath)
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			return "", fmt.Errorf("target path creation command %s failed: %v", createPathCmd, err)
		}
		// Set the command's stdout as the new target path.
		newTargetPath = strings.TrimSpace(string(out))
	} else if customCreateDir != nil {
		// Create the target path using the custom create dir function.
		newpath, err := customCreateDir(targetPath)
		if err != nil {
			return "", err
		}
		newTargetPath = newpath
	} else {
		// Create the target path using mkdir.
		fileInfo, err := os.Stat(targetPath)
		if err != nil {
			if !os.IsNotExist(err) {
				return "", err
			}
			if err := os.MkdirAll(targetPath, 0755); err != nil {
				return "", err
			}
			return targetPath, nil
		}

		if !fileInfo.IsDir() {
			return "", fmt.Errorf("Target location %s is not a directory", targetPath)
		}
		newTargetPath = targetPath
	}

	return newTargetPath, nil
}

// removeMountTargetLocation takes a target path parameter and removes the path
// using a custom command, custom function or falls back to the default removal
// by deleting the path on the host.
func removeMountTargetLocation(targetPath string, removePathCmd string, customRemovePath func(string) error, timeout int) error {
	if targetPath == "" {
		return nil
	}

	if removePathCmd != "" {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
		defer cancel()

		cmd := exec.CommandContext(ctx, removePathCmd, targetPath)
		cmd.Stderr = os.Stderr
		_, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("target path removal command %s failed: %v", removePathCmd, err)
		}
	} else if customRemovePath != nil {
		if err := customRemovePath(targetPath); err != nil {
			return err
		}
	} else {
		return os.RemoveAll(targetPath)
	}
	return nil
}

func loadSecrets(path string) (*CSISecrets, error) {
	var creds CSISecrets

	yamlFile, err := ioutil.ReadFile(path)
	if err != nil {
		return &creds, fmt.Errorf("failed to read file %q: #%v", path, err)
	}

	err = yaml.Unmarshal(yamlFile, &creds)
	if err != nil {
		return &creds, fmt.Errorf("error unmarshaling yaml: #%v", err)
	}

	return &creds, nil
}

var uniqueSuffix = "-" + pseudoUUID()

// pseudoUUID returns a unique string generated from random
// bytes, empty string in case of error.
func pseudoUUID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		// Shouldn't happen?!
		return ""
	}
	return fmt.Sprintf("%08X-%08X", b[0:4], b[4:8])
}

// uniqueString returns a unique string by appending a random
// number. In case of an error, just the prefix is returned, so it
// alone should already be fairly unique.
func uniqueString(prefix string) string {
	return prefix + uniqueSuffix
}
