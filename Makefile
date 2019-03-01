
.PHONY: build dist linux darwin windows buildall version cleardist clean

BIN=gitlab-copy
WINDOWS_BIN=${BIN}.exe
DISTDIR=dist
GCDIR=${DISTDIR}/${BIN}
#
VERSION=`git describe --tags --always`
GC_VERSION=${BIN}-${VERSION}
GC_DARWIN_AMD64=${GC_VERSION}-darwin-amd64
GC_FREEBSD_AMD64=${GC_VERSION}-freebsd-amd64
GC_LINUX_AMD64=${GC_VERSION}-linux-amd64
GC_WINDOWS_AMD64=${GC_VERSION}-windows-amd64
#
GB_BUILD64=GOARCH=amd64 go build
MAIN_CMD=github.com/gotsunami/${BIN}/cmd/${BIN}

all: build

build: version
	@go build -v -o bin/${BIN} ${MAIN_CMD}

test: version
	@go test ./... -coverprofile=/tmp/cover.out
	@go tool cover -html=/tmp/cover.out -o /tmp/coverage.html

coverage:
	@./tools/coverage.sh `pwd`

htmlcoverage:
	@./tools/coverage.sh --html `pwd`

dist: cleardist buildall zip sourcearchive 

zip: linux darwin freebsd windows
	@rm -rf ${GCDIR}

linux:
	@cp bin/${GC_VERSION}-linux* ${GCDIR}/${BIN} && \
		(cd ${DISTDIR} && zip -r ${GC_LINUX_AMD64}.zip ${BIN})

darwin:
	@cp bin/${GC_VERSION}-darwin* ${GCDIR}/${BIN} && \
		(cd ${DISTDIR} && zip -r ${GC_DARWIN_AMD64}.zip ${BIN})

windows:
	@cp bin/${GC_VERSION}-windows* ${GCDIR}/${WINDOWS_BIN} && \
		(cd ${DISTDIR} && rm ${BIN}/${BIN} && zip -r ${GC_WINDOWS_AMD64}.zip ${BIN})

freebsd:
	@cp bin/${GC_VERSION}-freebsd* ${GCDIR}/${BIN} && \
		(cd ${DISTDIR} && zip -r ${GC_FREEBSD_AMD64}.zip ${BIN})

buildall: version
	@GOOS=darwin ${GB_BUILD64} -v -o bin/${GC_DARWIN_AMD64} ${MAIN_CMD}
	@GOOS=freebsd ${GB_BUILD64} -v -o bin/${GC_FREEBSD_AMD64} ${MAIN_CMD}
	@GOOS=linux ${GB_BUILD64} -v -o bin/${GC_LINUX_AMD64} ${MAIN_CMD}
	@GOOS=windows ${GB_BUILD64} -v -o bin/${GC_WINDOWS_AMD64} ${MAIN_CMD}

sourcearchive: version
	@git archive --format=zip -o ${DISTDIR}/${VERSION}.zip ${VERSION}
	@echo ${DISTDIR}/${VERSION}.zip
	@git archive -o ${DISTDIR}/${VERSION}.tar ${VERSION}
	@gzip ${DISTDIR}/${VERSION}.tar
	@echo ${DISTDIR}/${VERSION}.tar.gz

version:
	@mkdir -p ${GCDIR}
	@./tools/version.sh

cleardist:
	@rm -rf ${DISTDIR} && mkdir -p ${GCDIR}

clean:
	@rm -rf bin pkg ${DISTDIR}
