#!/bin/bash

# Exit on error
set -e

# --- Configuration ---
# Default platforms to build
DEFAULT_PLATFORMS="linux/amd64,linux/arm64"
# Default Dockerfile location (relative to script location)
DEFAULT_DOCKERFILE="./dist.dockerfile"
# Default build context (relative to script location)
BUILD_CONTEXT="./"

# --- Argument Parsing ---
PLATFORMS="${DEFAULT_PLATFORMS}"
DOCKERFILE="${DEFAULT_DOCKERFILE}"
TAG=""
PUSH_IMAGE=false
LOAD_IMAGE=false

while [[ $# -gt 0 ]]; do
  key="$1"
  case $key in
    -p|--platforms)
      PLATFORMS="$2"
      shift # past argument
      shift # past value
      ;;
    -t|--tag)
      TAG="$2"
      shift # past argument
      shift # past value
      ;;
    -f|--dockerfile)
      DOCKERFILE="$2"
      shift # past argument
      shift # past value
      ;;
    --push)
      PUSH_IMAGE=true
      shift # past argument
      ;;
    --load)
      LOAD_IMAGE=true
      shift # past argument
      ;;
    -h|--help)
      echo "Usage: build.sh -t <image:tag> [-p <platforms>] [-f <dockerfile>] [--push] [--load]"
      echo "  -t, --tag        Required. Docker image tag (e.g., your-repo/net-init:latest)."
      echo "  -p, --platforms  Optional. Comma-separated list of platforms (default: ${DEFAULT_PLATFORMS})."
      echo "  -f, --dockerfile Optional. Path to the Dockerfile (default: ${DEFAULT_DOCKERFILE})."
      echo "  --push           Optional. Push the image(s) to the registry after building."
      echo "  --load           Optional. Load single-platform image into local Docker daemon (conflicts with --push and multi-platform)."
      exit 0
      ;;
    *)    # unknown option
      echo "Unknown option: $1"
      exit 1
      ;;
  esac
done

# --- Validation ---
if [ -z "$TAG" ]; then
  echo "Error: Image tag (-t or --tag) is required."
  exit 1
fi

# Check for conflicting flags with multi-platform builds
PLATFORM_COUNT=$(echo "${PLATFORMS}" | tr ',' '\n' | wc -l)
if [ "$PLATFORM_COUNT" -gt 1 ] && [ "$LOAD_IMAGE" = true ]; then
  echo "Error: --load can only be used when building for a single platform."
  exit 1
fi
if [ "$PUSH_IMAGE" = true ] && [ "$LOAD_IMAGE" = true ]; then
    echo "Error: --push and --load are mutually exclusive."
    exit 1
fi


# --- Check Buildx ---
if ! docker buildx version > /dev/null 2>&1; then
    echo "Error: docker buildx is not available or not configured."
    echo "Please ensure Docker is installed and buildx is enabled (e.g., 'docker buildx create --use default')."
    exit 1
fi
echo "Using docker buildx..."

# --- Build Command ---
echo "Building image '${TAG}' for platforms: ${PLATFORMS}"
echo "Using Dockerfile: ${DOCKERFILE}"
echo "Build context: ${BUILD_CONTEXT}"

# Construct buildx command
BUILD_CMD="docker buildx build --platform ${PLATFORMS} -t ${TAG} -f ${DOCKERFILE}"

# Add optional flags
if [ "$PUSH_IMAGE" = true ]; then
  BUILD_CMD="${BUILD_CMD} --push"
  echo "Will push image after build."
elif [ "$LOAD_IMAGE" = true ]; then
  BUILD_CMD="${BUILD_CMD} --load"
  echo "Will load image into local Docker daemon after build."
fi

# Add provenance flag (often needed for multi-arch)
BUILD_CMD="${BUILD_CMD} --provenance=false ${BUILD_CONTEXT}"

# --- Execute Build ---
echo "Executing: ${BUILD_CMD}"
eval ${BUILD_CMD} # Use eval to handle potential spaces in paths/tags correctly

echo "Build complete for ${TAG}."

# --- Post Build Info ---
if [ "$PUSH_IMAGE" = true ]; then
    echo "Image pushed to registry."
    echo "You can inspect the multi-arch image using: docker buildx imagetools inspect ${TAG}"
elif [ "$LOAD_IMAGE" = true ]; then
    echo "Image loaded into local Docker daemon."
    echo "You can use the image locally: docker run --rm ${TAG} --help"
else
    echo "Image built to buildx cache. Use --push to push to registry or --load (for single platform) to use locally."
fi

exit 0