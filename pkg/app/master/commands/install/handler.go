package install

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/docker-slim/go-update"
	log "github.com/sirupsen/logrus"

	"github.com/docker-slim/docker-slim/pkg/util/errutil"
	"github.com/docker-slim/docker-slim/pkg/util/fsutil"
	vinfo "github.com/docker-slim/docker-slim/pkg/version"
)

const (
	dockerCLIPluginDirSuffx = "/.docker/cli-plugins"
	masterAppName           = "docker-slim"
	sensorAppName           = "docker-slim-sensor"
	binDirName              = "/usr/local/bin"
)

// OnCommand implements the 'install' docker-slim command
func OnCommand(
	doDebug bool,
	statePath string,
	archiveState string,
	inContainer bool,
	isDSImage bool,
	binDir bool,
	dockerCLIPlugin bool) {
	logger := log.WithFields(log.Fields{"app": "docker-slim", "command": "install"})

	appPath, err := os.Executable()
	errutil.FailOn(err)
	appDirPath := filepath.Dir(appPath)

	if binDir {
		err := installToBinDir(logger, statePath, inContainer, isDSImage, appDirPath)
		if err != nil {
			fmt.Printf("docker-slim[install]: info=status message='error installing to bin dir'\n")
			fmt.Printf("docker-slim[install]: state=exited version=%s\n", vinfo.Current())
			return
		}

		fmt.Printf("docker-slim[install]: state=bin.dir.installed\n")

		//use the path from the bin dir, so installing docker CLI plugin symlinks to the right binaries
		appDirPath = binDirName
	}

	if dockerCLIPlugin {
		//create a symlink
		err := installDockerCLIPlugin(logger, statePath, inContainer, isDSImage, appDirPath)
		if err != nil {
			fmt.Printf("docker-slim[install]: info=status message='error installing as Docker CLI plugin'\n")
			fmt.Printf("docker-slim[install]: state=exited version=%s\n", vinfo.Current())
			return
		}

		fmt.Printf("docker-slim[install]: state=docker.cli.plugin.installed\n")
	}
}

func installToBinDir(logger *log.Entry, statePath string, inContainer, isDSImage bool, appDirPath string) error {
	if err := installRelease(logger, appDirPath, statePath, binDirName); err != nil {
		logger.Debugf("installToBinDir error: %v", err)
		return err
	}

	return nil
}

func symlinkBinaries(logger *log.Entry, appRootPath, symlinkRootPath string) error {
	symlinkMasterAppPath := filepath.Join(symlinkRootPath, masterAppName)
	symlinkSensorAppPath := filepath.Join(symlinkRootPath, sensorAppName)
	targetSensorAppPath := filepath.Join(appRootPath, sensorAppName)
	targetMasterAppPath := filepath.Join(appRootPath, masterAppName)

	//todo:
	//should not symlink the sensor because Docker CLI will treat it as an invalid plugin
	//need to improve sensor bin discovery from master app symlink
	err := os.Symlink(targetSensorAppPath, symlinkSensorAppPath)
	if err != nil {
		return err
	}

	err = os.Symlink(targetMasterAppPath, symlinkMasterAppPath)
	if err != nil {
		return err
	}

	return nil
}

func installDockerCLIPlugin(logger *log.Entry, statePath string, inContainer, isDSImage bool, appDirPath string) error {
	hd, _ := os.UserHomeDir()
	dockerCLIPluginDir := filepath.Join(hd, dockerCLIPluginDirSuffx)

	if !fsutil.Exists(dockerCLIPluginDir) {
		var dirMode os.FileMode = 0755
		err := os.MkdirAll(dockerCLIPluginDir, dirMode)
		if err != nil {
			return err
		}
	}

	if err := symlinkBinaries(logger, appDirPath, dockerCLIPluginDir); err != nil {
		logger.Debugf("installDockerCLIPlugin error: %v", err)
		return err
	}

	return nil
}

func installRelease(logger *log.Entry, appRootPath, statePath, targetRootPath string) error {
	targetMasterAppPath := filepath.Join(targetRootPath, masterAppName)
	targetSensorAppPath := filepath.Join(targetRootPath, sensorAppName)
	srcSensorAppPath := filepath.Join(appRootPath, sensorAppName)
	srcMasterAppPath := filepath.Join(appRootPath, masterAppName)

	err := updateFile(logger, srcSensorAppPath, targetSensorAppPath)
	if err != nil {
		return err
	}

	//will copy the sensor to the state dir if DS is installed in a bad non-shared location on Macs
	fsutil.PreparePostUpdateStateDir(statePath)

	err = updateFile(logger, srcMasterAppPath, targetMasterAppPath)
	if err != nil {
		return err
	}

	return nil
}

//copied from updater
func updateFile(logger *log.Entry, sourcePath, targetPath string) error {
	file, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer file.Close()

	options := update.Options{}
	if targetPath != "" {
		options.TargetPath = targetPath
	}

	err = update.Apply(file, options)
	if err != nil {
		if rerr := update.RollbackError(err); rerr != nil {
			logger.Debugf("install.updateFile(%s,%s): Failed to rollback from bad update: %v",
				sourcePath, targetPath, rerr)
		}
	}
	return err
}
