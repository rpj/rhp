#!/bin/bash
# wrapper script for `go build -buildmode=plugin` that symbolically 
# links in the required source file(s) and then cleans up when finished

# trick to silence pushd/popd from here:
# https://stackoverflow.com/questions/25288194/dont-display-pushd-popd-stack-across-several-bash-scripts-quiet-pushd-popd
pushd () {
    command pushd "$@" > /dev/null
}
popd () {
    command popd "$@" > /dev/null
}

PLUGIN_PATH=$1
REQ_SRC_FILES=(types.go)

# get absolute path in a portable way
WD="$( cd "$( dirname "${BASH_SOURCE[0]}" )" >/dev/null 2>&1 && pwd )"
PROJECT_ROOT="${WD}/.."
pushd $PROJECT_ROOT
PROJECT_ROOT=$PWD
popd

BUILD_DIR="${PROJECT_ROOT}/build/plugins"

if [ ! -z $BUILD_PLUGIN_OUTDIR ]; then
  BUILD_DIR="${PROJECT_ROOT}/$BUILD_PLUGIN_OUTDIR"
fi

mkdir -p $BUILD_DIR
pushd $BUILD_DIR
BUILD_DIR=$PWD
popd

echo "$0 building into '${BUILD_DIR}'"

if [ -z $PLUGIN_PATH ]; then
  echo -e "Must give plugin path as sole argument\n"
  exit -1
fi

PPATH="${PROJECT_ROOT}/${PLUGIN_PATH}"

if [ ! -d $PPATH ]; then
  echo -e "Plugin '${PLUGIN_PATH}' not found at '${PPATH}'\n"
  exit -1
fi

pushd $PPATH
echo "Building '$PPATH'..."

for srcFile in "${REQ_SRC_FILES[@]}"
do
  echo "ln -s \"${PROJECT_ROOT}/${srcFile}\" \"./${srcFile}\""
  ln -s "${PROJECT_ROOT}/${srcFile}" "./${srcFile}"
done

/usr/local/go/bin/go fmt
/usr/local/go/bin/go build -buildmode=plugin ${GOOPTS}
PNAME="$(basename $PLUGIN_PATH).so"
PDEST="$BUILD_DIR/${PNAME}"
mv $PNAME $PDEST
echo "Produced '${PDEST}'"

for srcFile in "${REQ_SRC_FILES[@]}"
do
  echo "rm ${srcFile}"
  rm ${srcFile}
done

popd