#!/bin/bash

set -e
# set -x # Uncomment for detailed debugging

# --- Configuration ---
NETINIT_BUILDER_IMG="net-init-builder-img"
# Define internal URLs/ports used within containers
SERVICE_A_HEALTH_URL="http://localhost:9090/health" # URL relative to inside service-a
SERVICE_A_APP_URL="http://localhost:8000"      # URL relative to inside service-a
SERVICE_B_INTERNAL_URL="http://localhost:80"   # URL relative to inside service-b

# --- Argument Parsing ---
CLEANUP_ENABLED=true
if [ "$1" == "--no-cleanup" ]; then
  CLEANUP_ENABLED=false
  echo "--- Cleanup disabled ---"
fi

# --- Helper: Print Logs on Failure ---
print_logs_on_failure() {
  echo "--- Error detected, printing last 10 log lines ---"
  echo "--- service-a logs ---"
  docker compose logs --tail=10 service-a || echo "Failed to get service-a logs."
  echo "--- service-b logs ---"
  docker compose logs --tail=10 service-b || echo "Failed to get service-b logs (may not be running)."
  echo "-------------------------------------------------"
}

# --- Cleanup Function ---
cleanup() {
  if [ "$CLEANUP_ENABLED" = true ]; then
    echo "--- Cleaning up Docker Compose environment ---"
    docker compose down -v --remove-orphans --timeout 30 || true
    echo "--- Cleanup complete ---"
  else
    echo "--- Skipping cleanup ---"
  fi
}
trap cleanup EXIT INT TERM

# --- Build Stage ---
echo "--- Building net-init builder image ---"
DOCKER_BUILDKIT=1 docker build -t ${NETINIT_BUILDER_IMG} ../ -f ../Dockerfile

echo "--- Building test application images ---"
DOCKER_BUILDKIT=1 docker compose build --build-arg NETINIT_IMAGE=${NETINIT_BUILDER_IMG}

# --- Test Execution ---
echo "--- Starting service-a (will wait for service-b) ---"
docker compose up -d service-a

echo "--- Waiting a few seconds for service-a container to start ---"
sleep 5

echo "--- Checking service-a health via exec (expecting 503 - waiting) ---"
MAX_RETRIES=10
RETRY_COUNT=0
HTTP_STATUS=0
until [ "$HTTP_STATUS" -eq 503 ]; do
  RETRY_COUNT=$((RETRY_COUNT + 1))
  if [ ${RETRY_COUNT} -gt ${MAX_RETRIES} ]; then
    echo "Error: service-a health check via exec (${SERVICE_A_HEALTH_URL}) did not return status 503 after ${MAX_RETRIES} retries. Last status: ${HTTP_STATUS}"
    print_logs_on_failure
    exit 1
  fi
  echo "Retry #${RETRY_COUNT} checking service-a health via exec for 503..."
  # Execute wget inside service-a, parse status code from stderr (-S)
  # Redirect stderr (2) to stdout (1) to grep it
  HTTP_STATUS=$(docker compose exec service-a \
    wget --spider -S --timeout=2 --tries=1 ${SERVICE_A_HEALTH_URL} 2>&1 | grep "HTTP/" | tail -n1 | awk '{print $2}' || echo "000")
  echo "Status from exec wget: ${HTTP_STATUS}"
  sleep 2
done
echo "--- service-a health check via exec returned 503 as expected ---"

echo "--- Starting service-b dependency ---"
docker compose up -d service-b

echo "--- Checking if nginx is listening inside service-b container (retrying max 20s) ---"
MAX_INTERNAL_RETRIES=10
INTERNAL_RETRY_COUNT=0
INTERNAL_CHECK_OK=false
until [ "$INTERNAL_CHECK_OK" = true ]; do
    INTERNAL_RETRY_COUNT=$((INTERNAL_RETRY_COUNT + 1))
    if [ ${INTERNAL_RETRY_COUNT} -gt ${MAX_INTERNAL_RETRIES} ]; then
        echo "--- FAILURE: Nginx did NOT respond internally on port 80 (${SERVICE_B_INTERNAL_URL}) after ${MAX_INTERNAL_RETRIES} retries ---"
        print_logs_on_failure
        exit 1
    fi
    echo "Retry #${INTERNAL_RETRY_COUNT} checking internal wget..."
    # Use wget inside the container via exec
    if docker compose exec service-b wget --quiet --tries=1 --spider ${SERVICE_B_INTERNAL_URL}; then
        echo "--- Nginx IS responding internally on port 80 ---"
        INTERNAL_CHECK_OK=true
    else
        echo "Internal check failed (exit code $?), retrying..."
        sleep 2 # Wait before retrying internal check
    fi
done

# No need to check service-b from host anymore, internal check is sufficient

echo "--- Waiting for service-a health check via exec to turn green (expecting 200 - max 15s) ---"
MAX_RETRIES=15
RETRY_COUNT=0
HTTP_STATUS=0
until [ "$HTTP_STATUS" -eq 200 ]; do
  RETRY_COUNT=$((RETRY_COUNT + 1))
  if [ ${RETRY_COUNT} -gt ${MAX_RETRIES} ]; then
    echo "Error: service-a health check via exec (${SERVICE_A_HEALTH_URL}) did not return status 200 after ${MAX_RETRIES} retries. Last status: ${HTTP_STATUS}"
    print_logs_on_failure
    exit 1
  fi
  echo "Retry #${RETRY_COUNT} checking service-a health via exec for 200..."
  HTTP_STATUS=$(docker compose exec service-a \
    wget --spider -S --timeout=2 --tries=1 ${SERVICE_A_HEALTH_URL} 2>&1 | grep "HTTP/" | tail -n1 | awk '{print $2}' || echo "000")
  echo "Status from exec wget: ${HTTP_STATUS}"
  sleep 1
done
echo "--- service-a health check via exec returned 200 OK as expected ---"

echo "--- Checking service-a application endpoint via exec (expecting env var value) ---"
MAX_RETRIES=5
RETRY_COUNT=0
APP_RESPONSE=""
until [ -n "$APP_RESPONSE" ]; do
    RETRY_COUNT=$((RETRY_COUNT + 1))
    if [ ${RETRY_COUNT} -gt ${MAX_RETRIES} ]; then
        echo "Error: Failed to get response from service-a app endpoint via exec (${SERVICE_A_APP_URL}) after ${MAX_RETRIES} retries."
        print_logs_on_failure
        exit 1
    fi
    echo "Retry #${RETRY_COUNT} checking service-a app endpoint via exec..."
    # Execute wget inside service-a and capture stdout (-O -)
    APP_RESPONSE=$(docker compose exec service-a \
      wget -q -O - --timeout=2 --tries=1 ${SERVICE_A_APP_URL} || echo "")
    sleep 1
done

EXPECTED_CONTENT="hello_netinit_compose"

if echo "${APP_RESPONSE}" | grep -q "${EXPECTED_CONTENT}"; then
  echo "--- SUCCESS: service-a application responded correctly with env var ---"
else
  echo "--- FAILURE: service-a application response mismatch ---"
  echo "Expected content containing: ${EXPECTED_CONTENT}"
  echo "Actual response: ${APP_RESPONSE}"
  print_logs_on_failure
  exit 1
fi

echo "--- Integration test PASSED! ---"

# Cleanup is handled by the trap EXIT (respecting CLEANUP_ENABLED)
exit 0