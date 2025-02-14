/*
Copyright (c) 2014 Ashley Jeffs

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/

package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"

	"github.com/jeffail/leaps/lib"
	"github.com/jeffail/leaps/lib/auth"
	"github.com/jeffail/leaps/lib/register"
	"github.com/jeffail/leaps/lib/store"
	"github.com/jeffail/leaps/net"
	"github.com/jeffail/util"
	"github.com/jeffail/util/log"
	"github.com/jeffail/util/metrics"
	"github.com/jeffail/util/path"
)

/*--------------------------------------------------------------------------------------------------
 */

/*
LeapsConfig - The all encompassing leaps configuration. Contains configurations for individual leaps
components, which determine the role of this leaps instance. Currently a stand alone leaps server is
the only supported role.
*/
type LeapsConfig struct {
	NumProcesses         int                      `json:"num_processes" yaml:"num_processes"`
	LoggerConfig         log.LoggerConfig         `json:"logger" yaml:"logger"`
	MetricsConfig        metrics.Config           `json:"metrics" yaml:"metrics"`
	StoreConfig          store.Config             `json:"storage" yaml:"storage"`
	AuthenticatorConfig  auth.Config              `json:"authenticator" yaml:"authenticator"`
	CuratorConfig        lib.CuratorConfig        `json:"curator" yaml:"curator"`
	HTTPServerConfig     net.HTTPServerConfig     `json:"http_server" yaml:"http_server"`
	InternalServerConfig net.InternalServerConfig `json:"admin_server" yaml:"admin_server"`
}

/*--------------------------------------------------------------------------------------------------
 */

var (
	sharePathOverride *string
)

func init() {
	sharePathOverride = flag.String("share", "", "Override the path for file system sharing configs")
}

/*--------------------------------------------------------------------------------------------------
 */

var errEndpointNotConfigured = errors.New("HTTP Endpoint API required but not configured")

type endpointsRegister struct {
	publicRegister  register.EndpointRegister
	privateRegister register.EndpointRegister
}

func newEndpointsRegister(public, private register.EndpointRegister) register.PubPrivEndpointRegister {
	return &endpointsRegister{
		publicRegister:  public,
		privateRegister: private,
	}
}

func (e *endpointsRegister) RegisterPublic(endpoint, description string, handler http.HandlerFunc) error {
	if e.publicRegister == nil {
		return errEndpointNotConfigured
	}
	e.publicRegister.Register(endpoint, description, handler)
	return nil
}

func (e *endpointsRegister) RegisterPrivate(endpoint, description string, handler http.HandlerFunc) error {
	if e.publicRegister == nil {
		return errEndpointNotConfigured
	}
	e.privateRegister.Register(endpoint, description, handler)
	return nil
}

/*--------------------------------------------------------------------------------------------------
 */

func main() {
	var (
		err       error
		closeChan = make(chan bool)
	)

	leapsConfig := LeapsConfig{
		NumProcesses:         runtime.NumCPU(),
		LoggerConfig:         log.DefaultLoggerConfig(),
		MetricsConfig:        metrics.NewConfig(),
		StoreConfig:          store.NewConfig(),
		AuthenticatorConfig:  auth.NewConfig(),
		CuratorConfig:        lib.DefaultCuratorConfig(),
		HTTPServerConfig:     net.DefaultHTTPServerConfig(),
		InternalServerConfig: net.NewInternalServerConfig(),
	}

	// A list of default config paths to check for if not explicitly defined
	defaultPaths := []string{}

	/* If we manage to get the path of our executable then we want to try and find config files
	 * relative to that path, we always check from the parent folder since we assume leaps is
	 * stored within the bin folder.
	 */
	if executablePath, err := path.BinaryPath(); err == nil {
		defaultPaths = append(defaultPaths, filepath.Join(executablePath, "..", "config.yaml"))
		defaultPaths = append(defaultPaths, filepath.Join(executablePath, "..", "config", "leaps.yaml"))
		defaultPaths = append(defaultPaths, filepath.Join(executablePath, "..", "config.json"))
		defaultPaths = append(defaultPaths, filepath.Join(executablePath, "..", "config", "leaps.json"))
	}

	defaultPaths = append(defaultPaths, []string{
		filepath.Join(".", "leaps.yaml"),
		filepath.Join(".", "leaps.json"),
		"/etc/leaps.yaml",
		"/etc/leaps.json",
		"/etc/leaps/config.yaml",
		"/etc/leaps/config.json",
	}...)

	// Load configuration etc
	if !util.Bootstrap(&leapsConfig, defaultPaths...) {
		return
	}

	if len(*sharePathOverride) > 0 {
		leapsConfig.AuthenticatorConfig.FileConfig.SharePath = *sharePathOverride
		leapsConfig.StoreConfig.StoreDirectory = *sharePathOverride
	}

	// Logging and stats aggregation
	logger := log.NewLogger(os.Stdout, leapsConfig.LoggerConfig)
	var stats metrics.Aggregator
	if s, err := metrics.New(leapsConfig.MetricsConfig); err == nil {
		stats = s
	} else {
		fmt.Fprintln(os.Stderr, fmt.Sprintf("Metrics init error: %v\n", err))
		return
	}
	defer stats.Close()

	fmt.Printf("Launching a leaps instance, use CTRL+C to close.\n\n")

	// Document storage engine
	documentStore, err := store.Factory(leapsConfig.StoreConfig)
	if err != nil {
		fmt.Fprintln(os.Stderr, fmt.Sprintf("Document store error: %v\n", err))
		return
	}

	// Authenticator
	authenticator, err := auth.Factory(leapsConfig.AuthenticatorConfig, logger, stats)
	if err != nil {
		fmt.Fprintln(os.Stderr, fmt.Sprintf("Authenticator error: %v\n", err))
		return
	}

	// Curator of documents
	curator, err := lib.NewCurator(leapsConfig.CuratorConfig, logger, stats, authenticator, documentStore)
	if err != nil {
		fmt.Fprintln(os.Stderr, fmt.Sprintf("Curator error: %v\n", err))
		return
	}
	defer curator.Close()

	// HTTP API
	leapHTTP, err := net.CreateHTTPServer(curator, leapsConfig.HTTPServerConfig, logger, stats)
	if err != nil {
		fmt.Fprintln(os.Stderr, fmt.Sprintf("HTTP error: %v\n", err))
		return
	}
	defer leapHTTP.Stop()

	go func() {
		if httperr := leapHTTP.Listen(); httperr != nil {
			fmt.Fprintln(os.Stderr, fmt.Sprintf("Http listen error: %v\n", httperr))
		}
		closeChan <- true
	}()

	var adminRegister register.EndpointRegister

	// Internal admin HTTP API
	if 0 < len(leapsConfig.InternalServerConfig.Address) {
		adminHTTP, err := net.NewInternalServer(curator, leapsConfig.InternalServerConfig, logger, stats)
		if err != nil {
			fmt.Fprintln(os.Stderr, fmt.Sprintf("Admin HTTP error: %v\n", err))
			return
		}
		adminRegister = adminHTTP

		go func() {
			if httperr := adminHTTP.Listen(); httperr != nil {
				fmt.Fprintln(os.Stderr, fmt.Sprintf("Admin HTTP listen error: %v\n", httperr))
			}
			closeChan <- true
		}()
	}

	// Register for allowing other components to set API endpoints.
	register := newEndpointsRegister(leapHTTP, adminRegister)
	if err = authenticator.RegisterHandlers(register); err != nil {
		fmt.Fprintln(os.Stderr, fmt.Sprintf("Register authentication endpoints failed: %v\n", err))
		return
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Wait for termination signal
	select {
	case <-sigChan:
	case <-closeChan:
	}
}

/*--------------------------------------------------------------------------------------------------
 */
