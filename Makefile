BINARY := lazyslurm

.PHONY: build test vet clean deploy install

build:
	CGO_ENABLED=0 go build -o $(BINARY) .

test:
	go test ./...

vet:
	go vet ./...

clean:
	rm -f $(BINARY)

# Copy the freshly built binary into ~/bin on our hpc via sftp batch mode.
# (Plain scp fails there: the restricted login shell mangles the
# destination path with "dest open ...: Failure". sftp lands in $HOME,
# so relative bin/ paths work. sftp put won't overwrite the existing
# binary, so remove it first; the leading '-' makes rm a no-op on
# first deploy when the file doesn't exist yet.)
deploy: build
	printf '%s\n' '-rm bin/lazyslurm' 'put $(BINARY) bin/' | sftp -b - hpc

install: deploy
