#! /bin/bash

set -e

SCRIPT_DIR=$(dirname $0)
language=${1:-go}
FAILURE=

# This script will create a scaffold for a given language
# Then build the docker image for that scaffold
# Then run it and verify that it runs correctly

export SCAFFOLD_DOCKER_IMAGE=${SCAFFOLD_DOCKER_IMAGE:-build}
export SCAFFOLD_DIR=${SCRIPT_DIR}/../scaffold

if [ "$SCAFFOLD_DOCKER_IMAGE" == "build" ]
then
  echo "Building scaffold docker image"
  (cd .. && make docker-build)
  export SCAFFOLD_DOCKER_IMAGE=cortex-axon-agent:local
fi

tmp_dir=$(mktemp -d)

echo "Updating scaffolds..."
make update-scaffolds

echo "Scaffolding project: $language"
# Run the scaffolder
cd ${SCRIPT_DIR}/../agent
project_name="test-project-$language"
go run main.go init --language $language --name "$project_name" --path $tmp_dir

echo "Building docker image for scaffold"

case "$(uname)" in
  Darwin)
    inplace_arg="-Ix"
    ;;
  Linux)
    inplace_arg="-ix"
    ;;
esac

# Normalize the handler name
case $language in
  go)
    sed $inplace_arg 's/myExampleIntervalHandler/myHandler/g ' $tmp_dir/$project_name/main.go
    rm -f $tmp_dir/$project_name/main.gox
    ;;
  python)
     sed $inplace_arg 's/my_handler/myHandler/g ' $tmp_dir/$project_name/main.py
     rm -f $tmp_dir/$project_name/main.pyx
    ;;
  *)
    echo "Unsupported language: $language"
    exit 1
    ;;
esac

cd $tmp_dir/$project_name
docker_tag="$project_name:local"

export GITHUB_TOKEN=$GITHUB_PASSWORD 
docker build --secret "id=github_token,env=GITHUB_TOKEN" --build-arg "GITHUB_ID=$GITHUB_USERNAME" -t $docker_tag .


echo "Running docker image for scaffold"
docker_http_port=9797
docker_hostname="docker-test-scaffold"
docker_id=$(docker run -e "DRYRUN=1" -e "HOSTNAME=$docker_hostname" -p "$docker_http_port:80" -d $docker_tag)

# Define a cleanup function
cleanup() {

  echo "Cleaning up..."
 
  if [ -n "$NO_CLEANUP_ON_FAIL" ] && [ -n "$FAILURE" ]
  then
    echo "Not cleaning up project because CLEANUP_ON_FAILURE is set to false"
    echo "Project directory: $tmp_dir/$project_name"
    return
  fi

  docker rm -f $docker_id || true
  rm -rf $tmp_dir
}

trap cleanup EXIT

echo "Sleeping for 5 seconds to let the docker image start"
sleep 5

echo "Checking that the docker image runs correctly"

echo "${docker_id}"

logs=$(docker logs $docker_id 2>&1)

echo $logs

if echo "$logs" | grep -i "handler called"
then
  echo "SUCCESS! Found expected output in logs"
else 
  echo "Failed to find expected output ("handler called") in logs"
  echo "Logs:"
  echo "$logs"
  FAILURE=true
  exit 1
fi

# Test handler listing
handler_list=$(docker exec $docker_id /agent/cortex-axon-agent handlers list)
echo "Handler list: $handler_list"

find_handler=$(echo "$handler_list" | grep -i "myHandler")
find_active=$(echo "$handler_list" | grep -i "Active: true")

if [ -n "$find_handler" ] && [ -n "$find_active" ]
then
  echo "SUCCESS! Got handler list and active status"
else 
  echo "Failed to get handler or active status"
  echo "Handler list:"
  echo "$handler_list"
  FAILURE=true
  exit 1
fi

echo "Testing handler history and logs"
execution_history=$(docker exec $docker_id /agent/cortex-axon-agent handlers history --logs myHandler)
history_items=$(echo "$execution_history" | grep -c myHandler || echo 0)
history_logs=$(echo "$execution_history" | grep -c "INFO" || echo 0)

if [ "$history_items" -gt 1 ] && [ "$history_logs" -gt 1 ]
then
  echo "SUCCESS! Got execution history and logs"
else
  echo "Failed to get execution history OR logs"
  echo "HISTORY_ITEMS count: $history_items"
  echo "HISTORY_LOGS count: $history_logs"
  echo "Execution history:"
  echo "$execution_history"
  FAILURE=true
  exit 1
fi

echo "testing http server"
response=$(curl -s http://localhost:$docker_http_port/__axon/info)

if echo "$response" | grep -q $docker_hostname
then
  echo "SUCCESS! Got expected response from http server"
else
  echo "Failed to get expected response from http server"
  echo "Response:"
  echo "$response"
  FAILURE=true
  exit 1
fi

# test metrics
response=$(curl -s http://localhost:$docker_http_port/metrics)

if echo "$response" | grep -q 'axon_http_requests{method="GET",path="/__axon/info",status="200"}'
then
  echo "SUCCESS! Got expected metrics response from http server"
else
  echo "Failed to get expected metrics response from http server"
  echo "Response:"
  echo "$response"
  FAILURE=true
  exit 1
fi