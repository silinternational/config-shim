package main

import (
	"bufio"
	"bytes"
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/appconfig"
	"github.com/aws/aws-sdk-go-v2/service/appconfigdata"
	"github.com/joho/godotenv"
)

var (
	verbose bool
	update  bool
)

type AppConfigParams struct {
	applicationID        string
	environmentID        string
	configProfileID      string
	deploymentStrategyID string
}

func main() {
	params := readFlags()

	configData, err := getConfig(params)
	if err != nil {
		fmt.Printf("failed to get config: %s\n", err)
		os.Exit(1)
	}

	if update {
		updatedConfigData, err := updateConfig(params, configData)
		if err != nil {
			fmt.Println(err.Error())
			os.Exit(1)
		}
		configData = []byte(updatedConfigData)
	}

	vars, err := getVars(configData)
	if err != nil {
		fmt.Printf("failed to get vars: %s\n", err)
		os.Exit(1)
	}

	args := flag.Args()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = append(os.Environ(), vars...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if verbose {
		fmt.Printf("running %q with args: %s and env: %s\n", args[0], args[1:], cmd.Env)
	}
	if err = cmd.Run(); err != nil {
		os.Exit(2)
	}
}

// readFlags gets CLI flags as needed for this command
func readFlags() (params AppConfigParams) {
	flag.StringVar(&params.applicationID, "app", "", "application identifier")
	flag.StringVar(&params.environmentID, "env", "", "environment identifier")
	flag.StringVar(&params.configProfileID, "config", "", "config profile identifier")
	flag.StringVar(&params.deploymentStrategyID, "strategy", "", "deployment strategy identifier")
	flag.BoolVar(&update, "u", false, "update config profile with value from environment")
	flag.BoolVar(&verbose, "v", false, "verbose output")
	flag.Parse()

	if params.applicationID == "" {
		fmt.Println("specify application identifier with --app flag")
		os.Exit(1)
	}

	if params.environmentID == "" {
		fmt.Println("specify environment identifier with --env flag")
		os.Exit(1)
	}

	if params.configProfileID == "" {
		fmt.Println("specify config profile identifier with --config flag")
		os.Exit(1)
	}

	if update && params.deploymentStrategyID == "" {
		fmt.Println("deployment strategy identifier is required for update mode, use --strategy flag")
		os.Exit(1)
	}

	if flag.NArg() == 0 {
		fmt.Println("must specify program to execute")
		os.Exit(1)
	}

	fmt.Printf("reading from AppConfig app %q, env %q, config profile %q\n",
		params.applicationID, params.environmentID, params.configProfileID)

	return
}

// getConfig gets the latest configuration from AWS AppConfig for the specified app, profile, and environment
func getConfig(params AppConfigParams) ([]byte, error) {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}

	client := appconfigdata.NewFromConfig(cfg)
	session, err := client.StartConfigurationSession(ctx, &appconfigdata.StartConfigurationSessionInput{
		ApplicationIdentifier:          &params.applicationID,
		ConfigurationProfileIdentifier: &params.configProfileID,
		EnvironmentIdentifier:          &params.environmentID,
	})
	if err != nil {
		return nil, err
	}

	configuration, err := client.GetLatestConfiguration(ctx, &appconfigdata.GetLatestConfigurationInput{
		ConfigurationToken: session.InitialConfigurationToken,
	})
	if err != nil {
		return nil, err
	}

	return configuration.Configuration, nil
}

// getVars parses a config in env format and returns a slice of variable-value strings like "VAR=value" suitable to
// supply to the Env attribute of the os/exec Cmd struct.
func getVars(config []byte) ([]string, error) {
	vars, err := godotenv.Parse(bytes.NewReader(config))
	if err != nil {
		return nil, fmt.Errorf("failed to parse configuration from AppConfig: %w", err)
	}

	fmt.Printf("read %d lines from AppConfig\n", len(vars))
	if verbose {
		fmt.Printf("vars: %s\n", vars)
	}

	varSlice := make([]string, 0, len(vars))
	for k, v := range vars {
		varSlice = append(varSlice, k+"="+v)
	}

	return varSlice, nil
}

// updateConfig looks in the config file for variables that should be updated from the local environment, swaps out
// the value, and sends the new config file to AWS AppConfig
func updateConfig(params AppConfigParams, cfgBytes []byte) ([]byte, error) {
	newCfg, err := replaceConfigValues(cfgBytes)
	if err != nil {
		return nil, fmt.Errorf("failure replacing values: %w", err)
	}

	err = deployNewConfig(params, newCfg)
	if err != nil {
		return nil, fmt.Errorf("failed to deploy config: %w", err)
	}

	if verbose {
		fmt.Printf("updated config: %s\n", newCfg)
	}
	return newCfg, nil
}

// replaceConfigValues substitutes values from the local environment into designated variables in the config file
func replaceConfigValues(cfg []byte) ([]byte, error) {
	localEnv := os.Environ()
	envVars, err := godotenv.Parse(strings.NewReader(strings.Join(localEnv, "\n")))
	if err != nil {
		return nil, fmt.Errorf("failed to parse environment variables using godotenv.Parse: %w", err)
	}

	scanner := bufio.NewScanner(bytes.NewReader(cfg))
	var output bytes.Buffer
	for scanner.Scan() {
		line := scanner.Text()
		for k, v := range envVars {
			var err error
			line, err = replaceLine(line, k, v)
			if err != nil {
				return nil, err
			}
		}
		output.WriteString(line + "\n")
	}

	if err = scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading input: %w", err)
	}
	return output.Bytes(), nil
}

// replaceLine handles one line of the config file, replacing the variable value if marked for update
func replaceLine(line, variable, newValue string) (string, error) {
	if !strings.HasPrefix(line, variable) {
		return line, nil
	}

	parts := strings.SplitN(line, "#", 2)
	if len(parts) != 2 {
		return line, nil
	}

	if !strings.Contains(parts[1], "{update}") {
		return line, nil
	}

	// this doesn't preserve style (whitespace and quote type or the absence of a quote) but that's fine for now
	line = fmt.Sprintf("%s='%s' #%s", variable, newValue, parts[1])

	if verbose {
		fmt.Printf("updated variable '%s' to '%s' in config file\n", variable, newValue)
	}
	return line, nil
}

// deployNewConfig submits a new config file to AWS AppConfig and starts a deployment for it
func deployNewConfig(params AppConfigParams, cfg []byte) error {
	ctx := context.Background()
	awsCfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return err
	}

	client := appconfig.NewFromConfig(awsCfg)

	createVersionInput := appconfig.CreateHostedConfigurationVersionInput{
		ApplicationId:          aws.String(params.applicationID),
		ConfigurationProfileId: aws.String(params.configProfileID),
		Content:                cfg,
		ContentType:            aws.String("text/plain"),
		Description:            aws.String("updated by config-shim"),
	}
	version, err := client.CreateHostedConfigurationVersion(ctx, &createVersionInput)
	if err != nil {
		return err
	}

	startDeploymentInput := appconfig.StartDeploymentInput{
		ApplicationId:          aws.String(params.applicationID),
		ConfigurationProfileId: aws.String(params.configProfileID),
		ConfigurationVersion:   aws.String(fmt.Sprintf("%d", version.VersionNumber)),
		DeploymentStrategyId:   aws.String(params.deploymentStrategyID),
		EnvironmentId:          aws.String(params.environmentID),
		Description:            aws.String("updated by config-shim"),
	}
	_, err = client.StartDeployment(ctx, &startDeploymentInput)
	if err != nil {
		return err
	}
	return nil
}
