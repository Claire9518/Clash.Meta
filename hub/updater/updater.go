package updater

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Dreamacro/clash/log"

	"github.com/AdguardTeam/golibs/errors"
)

// Updater is the AdGuard Home updater.
var (
	client http.Client

	version string
	channel string
	goarch  string
	goos    string
	goarm   string
	gomips  string

	workDir         string
	confName        string
	versionCheckURL string

	// mu protects all fields below.
	mu sync.RWMutex

	// TODO(a.garipov): See if all of these fields actually have to be in
	// this struct.
	currentExeName string // current binary executable
	updateDir      string // "workDir/agh-updater-v0.103.0"
	packageName    string // "workDir/agh-updater-v0.103.0/pkg_name.tar.gz"
	backupDir      string // "workDir/agh-backup"
	backupExeName  string // "workDir/agh-backup/AdGuardHome[.exe]"
	updateExeName  string // "workDir/agh-updater-v0.103.0/AdGuardHome[.exe]"
	unpackedFiles  []string

	newVersion string
	packageURL string

	// Cached fields to prevent too many API requests.
	prevCheckError error
	prevCheckTime  time.Time
	//prevCheckResult VersionInfo
)

// Config is the AdGuard Home updater configuration.
type Config struct {
	Client *http.Client

	Version string
	Channel string
	GOARCH  string
	GOOS    string
	GOARM   string
	GOMIPS  string

	// ConfName is the name of the current configuration file.  Typically,
	// "AdGuardHome.yaml".
	ConfName string
	// WorkDir is the working directory that is used for temporary files.
	WorkDir string
}

// Update performs the auto-updater.  It returns an error if the updater failed.
// If firstRun is true, it assumes the configuration file doesn't exist.
func Update(firstRun bool) (err error) {
	mu.Lock()
	defer mu.Unlock()
	packageURL = "https://github.com/MetaCubeX/Clash.Meta/releases/download/v1.14.2/clash.meta-windows-amd64-v1.14.2.zip"

	log.Infoln("updater: updating")
	defer func() {
		if err != nil {
			log.Errorln("updater: failed: %v", err)
		} else {
			log.Infoln("updater: finished")
		}
	}()

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("getting executable path: %w", err)
	}

	workDir = filepath.Dir(execPath)
	log.Debugln("workDir %s", execPath)

	err = prepare(execPath)
	if err != nil {
		return fmt.Errorf("preparing: %w", err)
	}

	//defer clean()

	err = downloadPackageFile()
	if err != nil {
		return fmt.Errorf("downloading package file: %w", err)
	}

	err = unpack()
	if err != nil {
		return fmt.Errorf("unpacking: %w", err)
	}

	err = backup(firstRun)
	if err != nil {
		return fmt.Errorf("making backup: %w", err)
	}

	err = replace()
	if err != nil {
		return fmt.Errorf("replacing: %w", err)
	}

	return nil
}

// VersionCheckURL returns the version check URL.
func VersionCheckURL() (vcu string) {
	mu.RLock()
	defer mu.RUnlock()

	return versionCheckURL
}

// prepare fills all necessary fields in Updater object.
func prepare(exePath string) (err error) {
	updateDir = filepath.Join(workDir, "meta-updater")

	_, pkgNameOnly := filepath.Split(packageURL)
	if pkgNameOnly == "" {
		return fmt.Errorf("invalid PackageURL: %q", packageURL)
	}

	packageName = filepath.Join(updateDir, pkgNameOnly)
	log.Debugln(packageName)
	backupDir = filepath.Join(workDir, "meta-backup")

	goos := runtime.GOOS

	if goos == "windows" {
		updateExeName = "clash.meta-windows-amd64.exe"
	}
	updateExeName = "clash.meta"

	backupExeName = filepath.Join(backupDir, filepath.Base(exePath))
	updateExeName = filepath.Join(updateDir, updateExeName)

	log.Debugln(
		"updater: updating from %s to %s using url: %s",
		//version.Version(),
		newVersion,
		packageURL,
	)

	currentExeName = exePath
	_, err = os.Stat(currentExeName)
	if err != nil {
		return fmt.Errorf("checking %q: %w", currentExeName, err)
	}

	return nil
}

// unpack extracts the files from the downloaded archive.
func unpack() error {
	var err error
	_, pkgNameOnly := filepath.Split(packageURL)

	log.Debugln("updater: unpacking package")
	if strings.HasSuffix(pkgNameOnly, ".zip") {
		unpackedFiles, err = zipFileUnpack(packageName, updateDir)
		if err != nil {
			return fmt.Errorf(".zip unpack failed: %w", err)
		}

	} else if strings.HasSuffix(pkgNameOnly, ".tar.gz") {
		unpackedFiles, err = tarGzFileUnpack(packageName, updateDir)
		if err != nil {
			return fmt.Errorf(".tar.gz unpack failed: %w", err)
		}

	} else {
		return fmt.Errorf("unknown package extension")
	}

	return nil
}

// backup makes a backup of the current configuration and supporting files.  It
// ignores the configuration file if firstRun is true.
func backup(firstRun bool) (err error) {
	log.Debugln("updater: backing up current configuration")
	_ = os.Mkdir(backupDir, 0777)

	wd := workDir
	err = copySupportingFiles(unpackedFiles, wd, backupDir)
	if err != nil {
		return fmt.Errorf("copySupportingFiles(%s, %s) failed: %w", wd, backupDir, err)
	}

	return nil
}

// replace moves the current executable with the updated one and also copies the
// supporting files.
func replace() error {
	err := copySupportingFiles(unpackedFiles, updateDir, workDir)
	if err != nil {
		return fmt.Errorf("copySupportingFiles(%s, %s) failed: %w", updateDir, workDir, err)
	}

	log.Debugln("updater: renaming: %s to %s", currentExeName, backupExeName)
	err = os.Rename(currentExeName, backupExeName)
	if err != nil {
		return err
	}

	if goos == "windows" {
		// rename fails with "File in use" error
		err = copyFile(updateExeName, currentExeName)
	} else {
		err = os.Rename(updateExeName, currentExeName)
	}
	if err != nil {
		return err
	}

	return nil
}

// clean removes the temporary directory itself and all it's contents.
func clean() {
	_ = os.RemoveAll(updateDir)
}

// MaxPackageFileSize is a maximum package file length in bytes. The largest
// package whose size is limited by this constant currently has the size of
// approximately 9 MiB.
const MaxPackageFileSize = 32 * 1024 * 1024

// Download package file and save it to disk
func downloadPackageFile() (err error) {
	var resp *http.Response
	resp, err = client.Get(packageURL)
	if err != nil {
		return fmt.Errorf("http request failed: %w", err)
	}
	defer func() { err = errors.WithDeferred(err, resp.Body.Close()) }()

	var r io.Reader
	r, err = LimitReader(resp.Body, MaxPackageFileSize)
	if err != nil {
		return fmt.Errorf("http request failed: %w", err)
	}

	log.Debugln("updater: reading http body")
	// This use of ReadAll is now safe, because we limited body's Reader.
	body, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("io.ReadAll() failed: %w", err)
	}

	log.Debugln("updateDir %s", updateDir)
	err = os.Mkdir(updateDir, 0o755)
	if err != nil {
		fmt.Errorf("mkdir error: %w", err)
	}

	log.Debugln("updater: saving package to file", packageName)
	err = os.WriteFile(packageName, body, 0o755)
	if err != nil {
		return fmt.Errorf("os.WriteFile() failed: %w", err)
	}
	return nil
}

func tarGzFileUnpackOne(outDir string, tr *tar.Reader, hdr *tar.Header) (name string, err error) {
	name = filepath.Base(hdr.Name)
	if name == "" {
		return "", nil
	}

	outputName := filepath.Join(outDir, name)

	if hdr.Typeflag == tar.TypeDir {
		if name == "AdGuardHome" {
			// Top-level AdGuardHome/.  Skip it.
			//
			// TODO(a.garipov): This whole package needs to be
			// rewritten and covered in more integration tests.  It
			// has weird assumptions and file mode issues.
			return "", nil
		}

		err = os.Mkdir(outputName, os.FileMode(hdr.Mode&0o755))
		if err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("os.Mkdir(%q): %w", outputName, err)
		}

		log.Debugln("updater: created directory %q", outputName)

		return "", nil
	}

	if hdr.Typeflag != tar.TypeReg {
		log.Infoln("updater: %s: unknown file type %d, skipping", name, hdr.Typeflag)

		return "", nil
	}

	var wc io.WriteCloser
	wc, err = os.OpenFile(
		outputName,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC,
		os.FileMode(hdr.Mode&0o755),
	)
	if err != nil {
		return "", fmt.Errorf("os.OpenFile(%s): %w", outputName, err)
	}
	defer func() { err = errors.WithDeferred(err, wc.Close()) }()

	_, err = io.Copy(wc, tr)
	if err != nil {
		return "", fmt.Errorf("io.Copy(): %w", err)
	}

	log.Debugln("updater: created file %q", outputName)

	return name, nil
}

// Unpack all files from .tar.gz file to the specified directory
// Existing files are overwritten
// All files are created inside outDir, subdirectories are not created
// Return the list of files (not directories) written
func tarGzFileUnpack(tarfile, outDir string) (files []string, err error) {
	f, err := os.Open(tarfile)
	if err != nil {
		return nil, fmt.Errorf("os.Open(): %w", err)
	}
	defer func() { err = errors.WithDeferred(err, f.Close()) }()

	gzReader, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("gzip.NewReader(): %w", err)
	}
	defer func() { err = errors.WithDeferred(err, gzReader.Close()) }()

	tarReader := tar.NewReader(gzReader)
	for {
		var hdr *tar.Header
		hdr, err = tarReader.Next()
		if errors.Is(err, io.EOF) {
			err = nil

			break
		} else if err != nil {
			err = fmt.Errorf("tarReader.Next(): %w", err)

			break
		}

		var name string
		name, err = tarGzFileUnpackOne(outDir, tarReader, hdr)

		if name != "" {
			files = append(files, name)
		}
	}

	return files, err
}

func zipFileUnpackOne(outDir string, zf *zip.File) (name string, err error) {
	var rc io.ReadCloser
	rc, err = zf.Open()
	if err != nil {
		return "", fmt.Errorf("zip file Open(): %w", err)
	}
	defer func() { err = errors.WithDeferred(err, rc.Close()) }()

	fi := zf.FileInfo()
	name = fi.Name()
	if name == "" {
		return "", nil
	}

	outputName := filepath.Join(outDir, name)
	if fi.IsDir() {
		if name == "AdGuardHome" {
			// Top-level AdGuardHome/.  Skip it.
			//
			// TODO(a.garipov): See the similar todo in
			// tarGzFileUnpack.
			return "", nil
		}

		err = os.Mkdir(outputName, fi.Mode())
		if err != nil && !errors.Is(err, os.ErrExist) {
			return "", fmt.Errorf("os.Mkdir(%q): %w", outputName, err)
		}

		log.Debugln("updater: created directory %q", outputName)

		return "", nil
	}

	var wc io.WriteCloser
	wc, err = os.OpenFile(outputName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode())
	if err != nil {
		return "", fmt.Errorf("os.OpenFile(): %w", err)
	}
	defer func() { err = errors.WithDeferred(err, wc.Close()) }()

	_, err = io.Copy(wc, rc)
	if err != nil {
		return "", fmt.Errorf("io.Copy(): %w", err)
	}

	log.Debugln("updater: created file %q", outputName)

	return name, nil
}

// Unpack all files from .zip file to the specified directory
// Existing files are overwritten
// All files are created inside 'outDir', subdirectories are not created
// Return the list of files (not directories) written
func zipFileUnpack(zipfile, outDir string) (files []string, err error) {
	zrc, err := zip.OpenReader(zipfile)
	if err != nil {
		return nil, fmt.Errorf("zip.OpenReader(): %w", err)
	}
	defer func() { err = errors.WithDeferred(err, zrc.Close()) }()

	for _, zf := range zrc.File {
		var name string
		name, err = zipFileUnpackOne(outDir, zf)
		if err != nil {
			break
		}

		if name != "" {
			files = append(files, name)
		}
	}

	return files, err
}

// Copy file on disk
func copyFile(src, dst string) error {
	d, e := os.ReadFile(src)
	if e != nil {
		return e
	}
	e = os.WriteFile(dst, d, 0o644)
	if e != nil {
		return e
	}
	return nil
}

func copySupportingFiles(files []string, srcdir, dstdir string) error {
	for _, f := range files {
		_, name := filepath.Split(f)
		if name == "AdGuardHome" || name == "AdGuardHome.exe" || name == "AdGuardHome.yaml" {
			continue
		}

		src := filepath.Join(srcdir, name)
		dst := filepath.Join(dstdir, name)

		err := copyFile(src, dst)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}

		log.Debugln("updater: copied: %q to %q", src, dst)
	}

	return nil
}
