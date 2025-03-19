#! /bin/bash

set -e  

SOURCE_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"


files=$(find $SOURCE_DIR/../server/snykbroker/accept_files -type f -name "accept*.json" | xargs -I{} basename "{}";)

# for each file, run the validation
for file in $files; do
    if ! docker run -v "${SOURCE_DIR}/../:/agent" 3scale/ajv validate -s /agent/test/filter.schema.json -d /agent/server/snykbroker/accept_files/$file;
    then
        echo "Validation failed for $file"
        exit 1
    fi
done

