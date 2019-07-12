// Copyright (c) 2015-present Mattermost, Inc. All Rights Reserved.
// See LICENSE.txt for license information.

package app

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path/filepath"

	"github.com/mattermost/mattermost-server/mlog"
	"github.com/mattermost/mattermost-server/model"
	"github.com/mattermost/mattermost-server/plugin"
	"github.com/mattermost/mattermost-server/utils"
)

func (a *App) InstallPluginFromData(data model.PluginEventData) {
	mlog.Error(fmt.Sprintf("InstallPluginFromData. ID: %v, Path: %v", data.PluginId, data.PluginPath))
}

func (a *App) RemovePluginFromData(data model.PluginEventData) {
	mlog.Error(fmt.Sprintf("RemovePluginFromData. ID: %v, Path: %v", data.PluginId, data.PluginPath))
}

// InstallPlugin unpacks and installs a plugin but does not enable or activate it.
func (a *App) InstallPlugin(pluginFile io.ReadSeeker, replace bool) (*model.Manifest, *model.AppError) {
	return a.installPlugin(pluginFile, replace)
}

func (a *App) installPlugin(pluginFile io.ReadSeeker, replace bool) (*model.Manifest, *model.AppError) {
	pluginsEnvironment := a.GetPluginsEnvironment()
	if pluginsEnvironment == nil {
		return nil, model.NewAppError("installPlugin", "app.plugin.disabled.app_error", nil, "", http.StatusNotImplemented)
	}

	tmpDir, err := ioutil.TempDir("", "plugintmp")
	if err != nil {
		return nil, model.NewAppError("installPlugin", "app.plugin.filesystem.app_error", nil, err.Error(), http.StatusInternalServerError)
	}
	defer os.RemoveAll(tmpDir)

	if err = utils.ExtractTarGz(pluginFile, tmpDir); err != nil {
		return nil, model.NewAppError("installPlugin", "app.plugin.extract.app_error", nil, err.Error(), http.StatusBadRequest)
	}

	tmpPluginDir := tmpDir
	dir, err := ioutil.ReadDir(tmpDir)
	if err != nil {
		return nil, model.NewAppError("installPlugin", "app.plugin.filesystem.app_error", nil, err.Error(), http.StatusInternalServerError)
	}

	if len(dir) == 1 && dir[0].IsDir() {
		tmpPluginDir = filepath.Join(tmpPluginDir, dir[0].Name())
	}

	manifest, _, err := model.FindManifest(tmpPluginDir)
	if err != nil {
		return nil, model.NewAppError("installPlugin", "app.plugin.manifest.app_error", nil, err.Error(), http.StatusBadRequest)
	}

	if !plugin.IsValidId(manifest.Id) {
		return nil, model.NewAppError("installPlugin", "app.plugin.invalid_id.app_error", map[string]interface{}{"Min": plugin.MinIdLength, "Max": plugin.MaxIdLength, "Regex": plugin.ValidIdRegex}, "", http.StatusBadRequest)
	}

	// Stash the previous state of the plugin, if available
	stashed := a.Config().PluginSettings.PluginStates[manifest.Id]

	bundles, err := pluginsEnvironment.Available()
	if err != nil {
		return nil, model.NewAppError("installPlugin", "app.plugin.install.app_error", nil, err.Error(), http.StatusInternalServerError)
	}

	// Check that there is no plugin with the same ID
	for _, bundle := range bundles {
		if bundle.Manifest != nil && bundle.Manifest.Id == manifest.Id {
			if !replace {
				return nil, model.NewAppError("installPlugin", "app.plugin.install_id.app_error", nil, "", http.StatusBadRequest)
			}

			if err := a.RemovePlugin(manifest.Id); err != nil {
				return nil, model.NewAppError("installPlugin", "app.plugin.install_id_failed_remove.app_error", nil, "", http.StatusBadRequest)
			}
		}
	}

	pluginPath := filepath.Join(*a.Config().PluginSettings.Directory, manifest.Id)
	err = utils.CopyDir(tmpPluginDir, pluginPath)
	if err != nil {
		return nil, model.NewAppError("installPlugin", "app.plugin.mvdir.app_error", nil, err.Error(), http.StatusInternalServerError)
	}

	// Store bundle in the file store to allow access from other servers.
	pluginFile.Seek(0, 0)

	storePluginFileName := filepath.Join("./plugins", manifest.Id) + ".tar.gz"
	if _, err := a.WriteFile(pluginFile, storePluginFileName); err != nil {
		return nil, model.NewAppError("uploadPlugin", "app.plugin.store_bundle.app_error", nil, err.Error(), http.StatusInternalServerError)
	}

	if stashed != nil && stashed.Enable {
		a.EnablePlugin(manifest.Id)
	}

	a.notifyClusterPluginEvent(
		model.CLUSTER_EVENT_INSTALL_PLUGIN,
		model.PluginEventData{
			PluginId:   manifest.Id,
			PluginPath: storePluginFileName,
		},
	)

	if err := a.notifyPluginStatusesChanged(); err != nil {
		mlog.Error("failed to notify plugin status changed", mlog.Err(err))
	}

	return manifest, nil
}

func (a *App) RemovePlugin(id string) *model.AppError {
	return a.removePlugin(id)
}

func (a *App) removePlugin(id string) *model.AppError {
	pluginsEnvironment := a.GetPluginsEnvironment()
	if pluginsEnvironment == nil {
		return model.NewAppError("removePlugin", "app.plugin.disabled.app_error", nil, "", http.StatusNotImplemented)
	}

	plugins, err := pluginsEnvironment.Available()
	if err != nil {
		return model.NewAppError("removePlugin", "app.plugin.deactivate.app_error", nil, err.Error(), http.StatusBadRequest)
	}

	var manifest *model.Manifest
	var pluginPath string
	for _, p := range plugins {
		if p.Manifest != nil && p.Manifest.Id == id {
			manifest = p.Manifest
			pluginPath = filepath.Dir(p.ManifestPath)
			break
		}
	}

	if manifest == nil {
		return model.NewAppError("removePlugin", "app.plugin.not_installed.app_error", nil, "", http.StatusBadRequest)
	}

	if pluginsEnvironment.IsActive(id) && manifest.HasClient() {
		message := model.NewWebSocketEvent(model.WEBSOCKET_EVENT_PLUGIN_DISABLED, "", "", "", nil)
		message.Add("manifest", manifest.ClientManifest())
		a.Publish(message)
	}

	pluginsEnvironment.Deactivate(id)
	pluginsEnvironment.RemovePlugin(id)
	a.UnregisterPluginCommands(id)

	err = os.RemoveAll(pluginPath)
	if err != nil {
		return model.NewAppError("removePlugin", "app.plugin.remove.app_error", nil, err.Error(), http.StatusInternalServerError)
	}

	// Remove bundle from the file store.
	storePluginFileName := filepath.Join("./plugins", manifest.Id) + ".tar.gz"
	bundleExist, fileErr := a.FileExists(storePluginFileName)
	if fileErr != nil {
		return model.NewAppError("removePlugin", "app.plugin.remove_bundle.app_error", nil, err.Error(), http.StatusInternalServerError)
	}

	if bundleExist {
		if err := a.RemoveFile(storePluginFileName); err != nil {
			return model.NewAppError("removePlugin", "app.plugin.remove_bundle.app_error", nil, err.Error(), http.StatusInternalServerError)
		}

		a.notifyClusterPluginEvent(
			model.CLUSTER_EVENT_REMOVE_PLUGIN,
			model.PluginEventData{
				PluginId:   manifest.Id,
				PluginPath: storePluginFileName,
			},
		)
	}

	return nil
}
