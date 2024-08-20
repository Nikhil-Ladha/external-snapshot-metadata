/*
Copyright 2024 The Kubernetes Authors.

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

package sidecar

import (
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"regexp"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/kubernetes-csi/external-snapshot-metadata/pkg/internal/runtime"
)

func TestSidecarFlagSet(t *testing.T) {
	t.Run("invalid-flags", func(t *testing.T) {
		defer saveAndResetGlobalState()()

		r, w, err := os.Pipe()
		assert.NoError(t, err)
		os.Stderr = w

		progName := "sidecar"
		argv := []string{progName, "-unknown-flag"}

		assert.Equal(t, flag.ExitOnError, sidecarFlagSetErrorHandling)
		sidecarFlagSetErrorHandling = flag.ContinueOnError // override for this test.

		// Invoke via Run.
		rc := Run(argv, "version")
		assert.Equal(t, 1, rc)

		assert.NoError(t, w.Close())
		output, err := io.ReadAll(r)
		assert.NoError(t, err)
		// (?s) makes . match \n within the group
		assert.Regexp(t, regexp.MustCompile(`(?s)unknown-flag.*Usage`), string(output))
	})

	t.Run("show-version", func(t *testing.T) {
		defer saveAndResetGlobalState()()

		r, w, err := os.Pipe()
		assert.NoError(t, err)
		os.Stdout = w

		progName := "sidecar"
		argv := []string{progName, "-version"}
		version := "sidecar-version"

		// Invoke via Run.
		rc := Run(argv, version)
		assert.Equal(t, 0, rc)

		assert.NoError(t, w.Close())
		output, err := io.ReadAll(r)
		assert.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("%s %s\n", progName, version), string(output))
	})

	t.Run("default-runtime-args", func(t *testing.T) {
		defer saveAndResetGlobalState()()

		expTLSCertFile := "/tls/certFile"
		t.Setenv(tlsCertEnvVar, expTLSCertFile)
		expTLSKeyFile := "/tls/keyFile"
		t.Setenv(tlsKeyEnvVar, expTLSKeyFile)

		argv := []string{"progName"}
		sfs := newSidecarFlagSet(argv[0], "version")

		hsv, err := sfs.parseFlagsAndHandleShowVersion(argv[1:])
		assert.NoError(t, err)
		assert.False(t, hsv)

		rta := sfs.runtimeArgsFromFlags()

		expRTA := runtime.Args{
			CSIAddress:   defaultCSISocket,
			CSITimeout:   defaultCSITimeout,
			KubeAPIBurst: defaultKubeAPIBurst,
			KubeAPIQPS:   defaultKubeAPIQPS,
			Kubeconfig:   defaultKubeconfig,
			GRPCPort:     defaultGRPCPort,
			TLSCertFile:  expTLSCertFile,
			TLSKeyFile:   expTLSKeyFile,
		}

		assert.Equal(t, expRTA, rta)
	})
}

func TestStartGRPCServerAndValidateCSIDriver(t *testing.T) {
	t.Run("server-creation-error", func(t *testing.T) {
		rth := runtime.NewTestHarness().WithTestTLSFiles(t).WithFakeKubeConfig(t).WithFakeCSIDriver(t, nil)
		defer rth.RemoveTestTLSFiles(t)
		defer rth.RemoveFakeKubeConfig(t)
		defer rth.TerminateFakeCSIDriver(t)

		rt := rth.RuntimeForFakeCSIDriver(t)
		rt.TLSCertFile += "foo" // invalid path

		// test via Run
		sfs := &sidecarFlagSet{}
		argv := sfs.runtimeArgsToArgv("progName", rt.Args)
		rc := Run(argv, "version")
		assert.Equal(t, 1, rc)
	})

	t.Run("start-server-error", func(t *testing.T) {
		rth := runtime.NewTestHarness().WithTestTLSFiles(t).WithFakeKubeConfig(t).WithFakeCSIDriver(t, nil)
		defer rth.RemoveTestTLSFiles(t)
		defer rth.RemoveFakeKubeConfig(t)
		defer rth.TerminateFakeCSIDriver(t)

		rt := rth.RuntimeForFakeCSIDriver(t)

		rt.GRPCPort = -1 // invalid port

		s, err := startGRPCServerAndValidateCSIDriver(rt)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "invalid port")
		assert.Nil(t, s)
	})

	t.Run("csi-driver-wait-timeout", func(t *testing.T) {
		rth := runtime.NewTestHarness().WithTestTLSFiles(t).WithFakeKubeConfig(t).WithFakeCSIDriver(t, nil)
		defer rth.RemoveTestTLSFiles(t)
		defer rth.RemoveFakeKubeConfig(t)
		defer rth.TerminateFakeCSIDriver(t)

		rt := rth.RuntimeForFakeCSIDriver(t)

		s, err := startGRPCServerAndValidateCSIDriver(rt)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "error waiting for CSI driver to become ready") // probe unimplemented.
		assert.Nil(t, s)
	})
}

func TestRun(t *testing.T) {
	t.Run("runtime-creation-error", func(t *testing.T) {
		// If the cluster Host/Port env variables are not set then a kubeconfig file is needed.
		t.Setenv("KUBERNETES_SERVICE_HOST", "")
		t.Setenv("KUBERNETES_SERVICE_PORT", "")

		argv := []string{"progName"}

		rc := Run(argv, "version")
		assert.Equal(t, 1, rc)
	})

	t.Run("launch-and-terminate", func(t *testing.T) {
		proc, err := os.FindProcess(syscall.Getpid())
		assert.NoError(t, err)

		// Specifying a fake snapshot metadata server to WithFakeCSIDriver()
		// makes the fake identity server advertise the needed capabilities.
		sms := &testSnapshotMetadataServer{}
		rth := runtime.NewTestHarness().WithTestTLSFiles(t).WithFakeKubeConfig(t).WithFakeCSIDriver(t, sms)
		defer rth.RemoveTestTLSFiles(t)
		defer rth.RemoveFakeKubeConfig(t)
		defer rth.TerminateFakeCSIDriver(t)

		// Still need to add a response to the fake identity server Probe.
		rth.FakeProbeResponse = &csi.ProbeResponse{Ready: wrapperspb.Bool(true)}

		rt := rth.RuntimeForFakeCSIDriver(t)

		sfs := &sidecarFlagSet{}
		argv := sfs.runtimeArgsToArgv("progName", rt.Args)

		// invoke Run() in a goroutine so as not to block.
		wg := sync.WaitGroup{}
		wg.Add(1)
		startedChan := make(chan int)

		rc := -1 // this will track the return value of Run().

		go func() {
			close(startedChan)
			rc = Run(argv, "version")
			wg.Done()
		}()

		<-startedChan

		// Send a termination signal to the server after a brief delay.
		// As there are multiple possible termination signals we randomly
		// select one each invocation.
		go func() {
			time.Sleep(time.Millisecond * 100)
			termSigIdx := rand.IntN(len(terminationSignals))
			proc.Signal(terminationSignals[termSigIdx])
		}()

		wg.Wait()

		assert.Equal(t, 0, rc)
	})
}

func saveAndResetGlobalState() func() {
	ss := struct {
		stdout               *os.File
		stderr               *os.File
		flagSetErrorHandling flag.ErrorHandling
	}{
		stdout:               os.Stdout,
		stderr:               os.Stderr,
		flagSetErrorHandling: sidecarFlagSetErrorHandling,
	}

	// return a restore function.
	return func() {
		os.Stdout = ss.stdout
		os.Stderr = ss.stderr
		sidecarFlagSetErrorHandling = ss.flagSetErrorHandling
	}
}

type testSnapshotMetadataServer struct {
	*csi.UnimplementedSnapshotMetadataServer
}

func (s *testSnapshotMetadataServer) GetMetadataAllocated(*csi.GetMetadataAllocatedRequest, csi.SnapshotMetadata_GetMetadataAllocatedServer) error {
	return status.Error(codes.Unimplemented, "GetMetadataAllocated is not implemented")
}

func (s *testSnapshotMetadataServer) GetMetadataDelta(*csi.GetMetadataDeltaRequest, csi.SnapshotMetadata_GetMetadataDeltaServer) error {
	return status.Error(codes.Unimplemented, "GetMetadataDelta is not implemented")
}