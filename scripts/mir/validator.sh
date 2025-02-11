#!/bin/bash

if [ $# -ne 1 ]
then
    echo "Provide the index of the validator to deploy as first argument. Starting from 0"
    exit 1
fi

INDEX=$1
EUDICO=${EUDICO:-./eudico}
CONFIG_DATA=${CONFIG_DATA/mir-config:-./scripts/mir/mir-config}

if [[ -z "${CONFIG_DATA}" ]]; then
  CONFIG_DATA=./scripts/mir/mir-config
else
  CONFIG_DATA="${CONFIG_DATA}/mir-config"
fi

LOG_LEVEL="info,mir-consensus=info,mir-manager=error"

# Config envs
export LOTUS_PATH=${LOTUS_PATH:-~/.lotus-local-net$INDEX}
export LOTUS_MINER_PATH=${LOTUS_MINER_PATH:-~/.lotus-miner-local-net$INDEX}
export LOTUS_SKIP_GENESIS_CHECK=_yes_
export CGO_CFLAGS_ALLOW="-D__BLST_PORTABLE__"
export CGO_CFLAGS="-D__BLST_PORTABLE__"
export GOLOG_LOG_LEVEL=$LOG_LEVEL

$EUDICO wait-api --timeout 120s

# Copy mir config and import keys
$EUDICO wallet import --as-default --format=json-lotus  $CONFIG_DATA/node$INDEX/wallet.key
cp $CONFIG_DATA/node$INDEX/* $LOTUS_PATH
mkdir $LOTUS_PATH/mir.db

# Set interceptor output
if [[ -z "${MIR_INTERCEPTOR_OUTPUT}" ]]; then
  n=`date '+%Y-%m-%dT%T'`
  MIR_INTERCEPTOR_OUTPUT="mir-event-logs/run/$INDEX/$n"
fi

# Run validator
$EUDICO mir validator run --nosync --max-block-delay="1s"
