#!/bin/sh -e

project=github.com/couchbaselabs/cbfs
top=`go list -f '{{.Dir}}' $project`
version=`git describe`

DIST=$top/dist

testpkg() {
    go test $project/...
    go vet $project/...
}

buildcbfs() {
    pkg=$project
    goflags="-v -ldflags '-X main.VERSION $version'"

    eval env GOARCH=386   GOOS=linux CGO_ENABLED=0 go build $goflags -o $DIST/cbfs.lin32 $pkg &
    eval env GOARCH=arm   GOOS=linux CGO_ENABLED=0 go build $goflags -o $DIST/cbfs.arm $pkg &
    eval env GOARCH=arm   GOARM=5 GOOS=linux CGO_ENABLED=0 go build $goflags -o $DIST/cbfs.arm5 $pkg &
    eval env GOARCH=amd64 GOOS=linux CGO_ENABLED=0 go build $goflags -o $DIST/cbfs.lin64 $pkg &
    eval env GOARCH=amd64 GOOS=freebsd CGO_ENABLED=0 go build $goflags -o $DIST/cbfs.fbsd $pkg &&
    eval env GOARCH=386   GOOS=windows go build $goflags -o $DIST/cbfs.win32.exe $pkg &
    eval env GOARCH=amd64 GOOS=windows go build $goflags -o $DIST/cbfs.win64.exe $pkg &
    eval env GOARCH=amd64 GOOS=darwin go build $goflags -o $DIST/cbfs.mac $pkg &
    
    wait
}

buildcbfsclient() {
    pkg=$project/cbfsclient
    goflags="-v -ldflags '-X main.VERSION $version'"

    eval env GOARCH=386   GOOS=linux CGO_ENABLED=0 go build $goflags -o $DIST/cbfsclient.lin32 $pkg &
    eval env GOARCH=arm   GOOS=linux CGO_ENABLED=0 go build $goflags -o $DIST/cbfsclient.arm $pkg &
    eval env GOARCH=arm   GOARM=5 GOOS=linux CGO_ENABLED=0 go build $goflags -o $DIST/cbfsclient.arm5 $pkg &
    eval env GOARCH=amd64 GOOS=linux CGO_ENABLED=0 go build $goflags -o $DIST/cbfsclient.lin64 $pkg &
    eval env GOARCH=amd64 GOOS=freebsd CGO_ENABLED=0 go build $goflags -o $DIST/cbfsclient.fbsd $pkg &&
    eval env GOARCH=386   GOOS=windows go build $goflags -o $DIST/cbfsclient.win32.exe $pkg &
    eval env GOARCH=amd64 GOOS=windows go build $goflags -o $DIST/cbfsclient.win64.exe $pkg &
    eval env GOARCH=amd64 GOOS=darwin go build $goflags -o $DIST/cbfsclient.mac $pkg &
    
    wait
}

compress() {
    rm -f $DIST/cbfs.*.gz $DIST/cbfsclient.*.gz || true
    for i in $DIST/cbfs.* $DIST/cbfsclient.*
    do
        gzip -9v $i &
    done

    wait
}

upload() {
    cbfsclient ${cbfsserver:-http://cbfs.hq.couchbase.com:8484/} upload \
        -ignore=$DIST/.cbfsclient.ignore -delete -v \
        $DIST/ dist/
}

testpkg
buildcbfs
buildcbfsclient
compress
upload
