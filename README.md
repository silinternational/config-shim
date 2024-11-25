# config-shim

Languages like PHP, when used in a web application environment are not well-suited for application configuration tools like AWS AppConfig. The entire application is initialized for every web request. This means that without persistent data storage, configuration must be fetched from the API on every request. This tool, provides for a quick migration from an environment-based configuration to AppConfig by reading from the config API once at the time the server starts. It passes configuration into environment variables in the process that starts your web server.

## Configuration

In your startup script insert a call to `config-shim` like this:

```shell
config-shim --app my_app --config default --env prod apache2ctl -D FOREGROUND
```

## Parameters

config-shim command-line parameter format is like `config-shim <flags> <command>`

### Flags
- `--app`: Application Identifier, can be the name of the application or the ID assigned by AWS
- `--config`: Configuration Profile Identifier, can be the profile name or the ID assigned by AWS
- `--env`: Environment Identifier, can be the name of the environment or the ID assigned by AWS
- `--strategy`: Deployment Strategy Identifier, must be the ID assigned by AWS. Only used in Update Mode.
- `-u`: Update mode. See the [Update Mode](#update-mode) section for details.
- `-v`: Verbose. Output more detail for debugging.

The application, configuration profile, and environment ID can be found in the AWS Console on the applicable page. However, the deployment strategy ID does not seem to be shown anywhere in the user interface. It can be found using the AWS CLI.

```shell
aws appconfig list-deployment-strategies
```

### Command
All parameters after the last flag are used as the command to execute after loading the environment variables from the config data received from AppConfig.

## Update Mode

Some configuration values may benefit from automatic updates driven by a CI/CD process, such as may be done during credential rotation. To facilitate this, config-shim can be used in "update mode". For example:

```shell
config-shim -u --strategy anqvp9a --app zj12skn --env m8ffu04 --config p69liya apache2ctl -D FOREGROUND
```

In this mode, config-shim will scan the configuration data for comments containing the string `{update}`. Lines containing this string will be updated to the value of the variable in the execution environment. The updated configuration data will be pushed to AWS AppConfig and a deployment will be started using the given deployment strategy. The updated configuration data is also applied to the child process.
