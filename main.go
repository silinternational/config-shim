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
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/joho/godotenv"
)

var (
	verbose bool
	update  bool
)

type ConfigParams struct {
	applicationID        string
	environmentID        string
	configProfileID      string
	deploymentStrategyID string
	path                 string
}

func main() {
	params, err := readFlags()
	if err != nil {
		fmt.Println("Error: " + err.Error())
		os.Exit(1)
	}

	if flag.NArg() == 0 {
		fmt.Println("Error: must specify program to execute")
		os.Exit(1)
	}

	var vars []string
	getConfigFunction := getConfigFromPS
	if params.path == "" {
		getConfigFunction = getConfigFromAppConfig
	}

	vars, err = getConfigFunction(params)
	if err != nil {
		fmt.Println("Error: " + err.Error())
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
		fmt.Printf("Error: command failed: %s\n", err)
		os.Exit(2)
	}
}

// readFlags gets CLI flags as needed for this command
func readFlags() (ConfigParams, error) {
	var params ConfigParams
	flag.StringVar(&params.applicationID, "app", "", "AppConfig application identifier")
	flag.StringVar(&params.environmentID, "env", "", "AppConfig environment identifier")
	flag.StringVar(&params.configProfileID, "config", "", "AppConfig config profile identifier")
	flag.StringVar(&params.deploymentStrategyID, "strategy", "", "AppConfig deployment strategy identifier")

	flag.StringVar(&params.path, "path", "", "Parameter Store base configuration path")

	flag.BoolVar(&update, "u", false, "update config profile with value from environment")
	flag.BoolVar(&verbose, "v", false, "verbose output")
	flag.Parse()

	if params.path != "" {
		fmt.Printf("reading from Parameter Store path %q\n", params.path)
		return params, nil
	}

	if params.applicationID == "" {
		return params, fmt.Errorf("no application ID provided. Specify with --app flag")
	}

	if params.environmentID == "" {
		return params, fmt.Errorf("no environment ID provided. Specify with --env flag")
	}

	if params.configProfileID == "" {
		return params, fmt.Errorf("no config profile ID provided. Specify with --config flag")
	}

	if update && params.deploymentStrategyID == "" {
		return params, fmt.Errorf("deployment strategy ID is required for update mode. Use --strategy flag")
	}

	fmt.Printf("reading from AppConfig app %q, env %q, config profile %q\n",
		params.applicationID, params.environmentID, params.configProfileID)

	return params, nil
}

// getConfigFromAppConfig retrieves all parameters from the AppConfig and returns them as a slice of string, where each
// string is of the form "param=value"
func getConfigFromAppConfig(params ConfigParams) ([]string, error) {
	configData, err := getLatestConfig(params)
	if err != nil {
		return nil, fmt.Errorf("failed to get config: %w", err)
	}

	if update {
		updatedConfigData, err := updateConfig(params, configData)
		if err != nil {
			return nil, err
		}
		configData = updatedConfigData
	}

	vars, err := getVars(configData)
	if err != nil {
		return nil, fmt.Errorf("failed to get vars: %w", err)
	}
	return vars, nil
}

// getLatestConfig gets the latest configuration from AWS AppConfig for the specified app, profile, and environment
func getLatestConfig(params ConfigParams) ([]byte, error) {
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
func updateConfig(params ConfigParams, configData []byte) ([]byte, error) {
	newCfg, err := replaceConfigValues(configData)
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
func deployNewConfig(params ConfigParams, cfg []byte) error {
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

// getConfigFromPS retrieves all parameters from the given path on Parameter Store and returns them as a slice of
// string, where each string is of the form "param=value"
func getConfigFromPS(p ConfigParams) ([]string, error) {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, err
	}

	client := ssm.NewFromConfig(cfg)

	out, err := client.GetParametersByPath(context.Background(), &ssm.GetParametersByPathInput{
		Path:           &p.path,
		WithDecryption: aws.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get parameters from SSM: %w", err)
	}

	vars := make([]string, 0, len(out.Parameters))
	for _, v := range out.Parameters {
		if v.Name == nil {
			_, _ = fmt.Fprintf(os.Stderr, "SSM returned a parameter with nil name")
			continue
		}
		name := strings.TrimPrefix(*v.Name, p.path)

		if v.Value == nil {
			_, _ = fmt.Fprintf(os.Stderr, "SSM returned parameter with nil value: %q", name)
			continue
		}

		vars = append(vars, name+"="+(*v.Value))
		_, _ = fmt.Fprintf(os.Stderr, "parameter read from SSM Parameter Store: %q", name)
	}
	return vars, nil
}
