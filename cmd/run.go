// ------------------------------------------------------------
// Copyright (c) Microsoft Corporation and Dapr Contributors.
// Licensed under the MIT License.
// ------------------------------------------------------------

package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dapr/cli/pkg/metadata"
	"github.com/dapr/cli/pkg/print"
	"github.com/dapr/cli/pkg/standalone"
	"github.com/dapr/cli/utils"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var (
	appPort         int
	profilePort     int
	appID           string
	configFile      string
	port            int
	grpcPort        int
	maxConcurrency  int
	enableProfiling bool
	logLevel        string
	protocol        string
	componentsPath  string
	appSSL          bool
	metricsPort     int
)

const (
	runtimeWaitTimeoutInSeconds = 60
)

var RunCmd = &cobra.Command{
	Use:   "run",
	Short: "Run Dapr and (optionally) your application side by side. Supported platforms: Self-hosted",
	Example: `
# Run a .NET application:
  dapr run --app-id myapp --app-port 5000 -- dotnet run
# Run a Java application:
  dapr run --app-id myapp -- java -jar myapp.jar
# Run a NodeJs application that listens to port 3000:
  dapr run --app-id myapp --app-port 3000 -- node myapp.js
# Run a Python application:
  dapr run --app-id myapp -- python myapp.py
# Run sidecar only:
  dapr run --app-id myapp
  `,
	Args: cobra.MinimumNArgs(0),
	PreRun: func(cmd *cobra.Command, args []string) {
		viper.BindPFlag("placement-host-address", cmd.Flags().Lookup("placement-host-address"))
	},
	Run: func(cmd *cobra.Command, args []string) {
		if len(args) == 0 {
			fmt.Println(print.WhiteBold("WARNING: no application command found."))
		}

		output, err := standalone.Run(&standalone.RunConfig{
			AppID:           appID,
			AppPort:         appPort,
			HTTPPort:        port,
			GRPCPort:        grpcPort,
			ConfigFile:      configFile,
			Arguments:       args,
			EnableProfiling: enableProfiling,
			ProfilePort:     profilePort,
			LogLevel:        logLevel,
			MaxConcurrency:  maxConcurrency,
			Protocol:        protocol,
			PlacementHost:   viper.GetString("placement-host-address"),
			ComponentsPath:  componentsPath,
			AppSSL:          appSSL,
			MetricsPort:     metricsPort,
		})
		if err != nil {
			print.FailureStatusEvent(os.Stdout, err.Error())
			return
		}

		sigCh := make(chan os.Signal, 1)
		setupShutdownNotify(sigCh)

		daprRunning := make(chan bool, 1)
		appRunning := make(chan bool, 1)

		go func() {
			print.InfoStatusEvent(
				os.Stdout,
				fmt.Sprintf(
					"Starting Dapr with id %s. HTTP Port: %v. gRPC Port: %v",
					output.AppID,
					output.DaprHTTPPort,
					output.DaprGRPCPort))

			output.DaprCMD.Stdout = os.Stdout
			output.DaprCMD.Stderr = os.Stderr

			err = output.DaprCMD.Start()
			if err != nil {
				print.FailureStatusEvent(os.Stdout, err.Error())
				os.Exit(1)
			}

			go func() {
				daprdErr := output.DaprCMD.Wait()

				if daprdErr != nil {
					print.FailureStatusEvent(os.Stdout, "The daprd process exited with error code: %s", daprdErr.Error())

				} else {
					print.SuccessStatusEvent(os.Stdout, "Exited Dapr successfully")
				}
				sigCh <- os.Interrupt
			}()

			if appPort <= 0 {
				// If app does not listen to port, we can check for Dapr's sidecar health before starting the app.
				// Otherwise, it creates a deadlock.
				sidecarUp := true
				print.InfoStatusEvent(os.Stdout, "Checking if Dapr sidecar is listening on HTTP port %v", output.DaprHTTPPort)
				err = utils.IsDaprListeningOnPort(output.DaprHTTPPort, time.Duration(runtimeWaitTimeoutInSeconds)*time.Second)
				if err != nil {
					sidecarUp = false
					print.WarningStatusEvent(os.Stdout, "Dapr sidecar is not listening on HTTP port: %s", err.Error())
				}

				print.InfoStatusEvent(os.Stdout, "Checking if Dapr sidecar is listening on GRPC port %v", output.DaprGRPCPort)
				err = utils.IsDaprListeningOnPort(output.DaprGRPCPort, time.Duration(runtimeWaitTimeoutInSeconds)*time.Second)
				if err != nil {
					sidecarUp = false
					print.WarningStatusEvent(os.Stdout, "Dapr sidecar is not listening on GRPC port: %s", err.Error())
				}

				if sidecarUp {
					print.InfoStatusEvent(os.Stdout, "Dapr sidecar is up and running.")
				} else {
					print.WarningStatusEvent(os.Stdout, "Dapr sidecar might not be responding.")
				}
			}

			daprRunning <- true
		}()

		<-daprRunning

		go func() {
			if output.AppCMD == nil {
				appRunning <- true
				return
			}

			stdErrPipe, pipeErr := output.AppCMD.StderrPipe()
			if pipeErr != nil {
				print.FailureStatusEvent(os.Stdout, fmt.Sprintf("Error creating stderr for App: %s", err.Error()))
				appRunning <- false
				return
			}

			stdOutPipe, pipeErr := output.AppCMD.StdoutPipe()
			if pipeErr != nil {
				print.FailureStatusEvent(os.Stdout, fmt.Sprintf("Error creating stdout for App: %s", err.Error()))
				appRunning <- false
				return
			}

			errScanner := bufio.NewScanner(stdErrPipe)
			outScanner := bufio.NewScanner(stdOutPipe)
			go func() {
				for errScanner.Scan() {
					fmt.Println(print.Blue(fmt.Sprintf("== APP == %s", errScanner.Text())))
				}
			}()

			go func() {
				for outScanner.Scan() {
					fmt.Println(print.Blue(fmt.Sprintf("== APP == %s", outScanner.Text())))
				}
			}()

			err = output.AppCMD.Start()
			if err != nil {
				print.FailureStatusEvent(os.Stdout, err.Error())
				appRunning <- false
				return
			}

			go func() {
				appErr := output.AppCMD.Wait()

				if appErr != nil {
					print.FailureStatusEvent(os.Stdout, "The App process exited with error code: %s", appErr.Error())
				} else {
					print.SuccessStatusEvent(os.Stdout, "Exited App successfully")
				}
				sigCh <- os.Interrupt
			}()

			appRunning <- true
		}()

		appRunStatus := <-appRunning
		if !appRunStatus {
			// Start App failed, try to stop Dapr and exit.
			err = output.DaprCMD.Process.Kill()
			if err != nil {
				print.FailureStatusEvent(os.Stdout, fmt.Sprintf("Start App failed, try to stop Dapr Error: %s", err))
			} else {
				print.SuccessStatusEvent(os.Stdout, "Start App failed, try to stop Dapr successfully")
			}
			os.Exit(1)
		}

		// Metadata API is only available if app has started listening to port, so wait for app to start before calling metadata API.
		err = metadata.Put(output.DaprHTTPPort, "cliPID", strconv.Itoa(os.Getpid()))
		if err != nil {
			print.WarningStatusEvent(os.Stdout, "Could not update sidecar metadata for cliPID: %s", err.Error())
		}

		if output.AppCMD != nil {
			appCommand := strings.Join(args, " ")
			print.InfoStatusEvent(os.Stdout, fmt.Sprintf("Updating metadata for app command: %s", appCommand))
			err = metadata.Put(output.DaprHTTPPort, "appCommand", appCommand)
			if err != nil {
				print.WarningStatusEvent(os.Stdout, "Could not update sidecar metadata for appCommand: %s", err.Error())
			} else {
				print.SuccessStatusEvent(os.Stdout, "You're up and running! Both Dapr and your app logs will appear here.\n")
			}
		} else {
			print.SuccessStatusEvent(os.Stdout, "You're up and running! Dapr logs will appear here.\n")
		}

		<-sigCh
		print.InfoStatusEvent(os.Stdout, "\nterminated signal received: shutting down")

		if output.DaprCMD.ProcessState == nil || !output.DaprCMD.ProcessState.Exited() {
			err = output.DaprCMD.Process.Kill()
			if err != nil {
				print.FailureStatusEvent(os.Stdout, fmt.Sprintf("Error exiting Dapr: %s", err))
			} else {
				print.SuccessStatusEvent(os.Stdout, "Exited Dapr successfully")
			}
		}

		if output.AppCMD != nil && (output.AppCMD.ProcessState == nil || !output.AppCMD.ProcessState.Exited()) {
			err = output.AppCMD.Process.Kill()
			if err != nil {
				print.FailureStatusEvent(os.Stdout, fmt.Sprintf("Error exiting App: %s", err))
			} else {
				print.SuccessStatusEvent(os.Stdout, "Exited App successfully")
			}
		}
	},
}

func init() {
	RunCmd.Flags().IntVarP(&appPort, "app-port", "p", -1, "The port your application is listening on")
	RunCmd.Flags().StringVarP(&appID, "app-id", "a", "", "The id for your application, used for service discovery")
	RunCmd.Flags().StringVarP(&configFile, "config", "c", standalone.DefaultConfigFilePath(), "Dapr configuration file")
	RunCmd.Flags().IntVarP(&port, "dapr-http-port", "H", -1, "The HTTP port for Dapr to listen on")
	RunCmd.Flags().IntVarP(&grpcPort, "dapr-grpc-port", "G", -1, "The gRPC port for Dapr to listen on")
	RunCmd.Flags().BoolVar(&enableProfiling, "enable-profiling", false, "Enable pprof profiling via an HTTP endpoint")
	RunCmd.Flags().IntVarP(&profilePort, "profile-port", "", -1, "The port for the profile server to listen on")
	RunCmd.Flags().StringVarP(&logLevel, "log-level", "", "info", "The log verbosity. Valid values are: debug, info, warn, error, fatal, or panic")
	RunCmd.Flags().IntVarP(&maxConcurrency, "app-max-concurrency", "", -1, "The concurrency level of the application, otherwise is unlimited")
	RunCmd.Flags().StringVarP(&protocol, "app-protocol", "P", "http", "The protocol (gRPC or HTTP) Dapr uses to talk to the application")
	RunCmd.Flags().StringVarP(&componentsPath, "components-path", "d", standalone.DefaultComponentsDirPath(), "The path for components directory")
	RunCmd.Flags().String("placement-host-address", "localhost", "The host on which the placement service resides")
	RunCmd.Flags().BoolVar(&appSSL, "app-ssl", false, "Enable https when Dapr invokes the application")
	RunCmd.Flags().IntVarP(&metricsPort, "metrics-port", "M", -1, "The port of metrics on dapr")
	RunCmd.Flags().BoolP("help", "h", false, "Print this help message")
	RootCmd.AddCommand(RunCmd)
}
