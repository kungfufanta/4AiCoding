# Docker Deployment

## Docker Build
Docker build is the process of creating a Docker image from a Dockerfile. A Dockerfile is a text document that contains all the commands to assemble an image. The command used is:
```bash
docker build -t <image-name>:<tag> .
```
This command takes the current directory (denoted by '.') containing the Dockerfile and builds an image using the specified name and tag.

## Docker Compose
Docker Compose is a tool for defining and running multi-container Docker applications. You can define your application’s services, networks, and volumes in a single file (docker-compose.yml) and manage them together. Start your application with:
```bash
docker-compose up
```
This command builds, (re)creates, starts, and attaches to containers for a service defined in the docker-compose.yml file.

## Container Testing
Container testing involves validating that the application running inside a container works correctly. You can accomplish this by executing functional, integration, and end-to-end tests that target the application within its containerized environment. Tools such as `Testcontainers` can facilitate the execution of tests against containerized dependencies.

## Health Checks
Health checks are crucial for ensuring that your containerized applications are functioning as expected. You can define a health check in your Dockerfile using the following command:
```dockerfile
HEALTHCHECK --interval=30s --timeout=30s --retries=5 CMD curl -f http://localhost/ || exit 1
```
This command will check the container's health every 30 seconds, timing out after 30 seconds, and retrying up to 5 times before considering the container unhealthy.