package tarexport

import (
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"

	"github.com/Sirupsen/logrus"
	"github.com/docker/docker/image"
	"github.com/docker/docker/image/v1"
	"github.com/docker/docker/layer"
	"github.com/docker/docker/pkg/archive"
	"github.com/docker/docker/pkg/chrootarchive"
	"github.com/docker/docker/pkg/progress"
	"github.com/docker/docker/pkg/streamformatter"
	"github.com/docker/docker/pkg/stringid"
	"github.com/docker/docker/pkg/symlink"
	"github.com/docker/docker/reference"
	digest "github.com/opencontainers/go-digest"
	ociv1 "github.com/opencontainers/image-spec/specs-go/v1"
)

func (l *tarexporter) Load(inTar io.ReadCloser, name string, refs map[string]string, outStream io.Writer) error {
	// add progress for load image
	var (
		sf             = streamformatter.NewJSONStreamFormatter()
		progressOutput progress.Output
	)

	progressOutput = sf.NewProgressOutput(outStream, false)

	tmpDir, err := ioutil.TempDir("", "docker-import-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmpDir)

	if err := chrootarchive.Untar(inTar, tmpDir, nil); err != nil {
		return err
	}

	// check and try to load an OCI image layout
	ociLayoutPath, err := safePath(tmpDir, "oci-layout")
	if err != nil {
		return err
	}
	ociLayoutFile, err := os.Open(ociLayoutPath)
	if err == nil {
		ociLayoutFile.Close()
		return l.ociLoad(tmpDir, name, refs, outStream, progressOutput)
	}

	// read manifest, if no file then load in legacy mode
	manifestPath, err := safePath(tmpDir, manifestFileName)
	if err != nil {
		return err
	}
	manifestFile, err := os.Open(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return l.legacyLoad(tmpDir, outStream, progressOutput)
		}
		return manifestFile.Close()
	}
	defer manifestFile.Close()

	var manifest []manifestItem
	if err := json.NewDecoder(manifestFile).Decode(&manifest); err != nil {
		return err
	}

	return l.loadHelper(tmpDir, manifest, outStream, progressOutput)
}

func (l *tarexporter) loadHelper(tmpDir string, manifests []manifestItem, outStream io.Writer, progressOutput progress.Output) error {
	for _, m := range manifests {
		configPath, err := safePath(tmpDir, m.Config)
		if err != nil {
			return err
		}
		config, err := ioutil.ReadFile(configPath)
		if err != nil {
			return err
		}
		img, err := image.NewFromJSON(config)
		if err != nil {
			return err
		}
		var rootFS image.RootFS
		rootFS = *img.RootFS
		rootFS.DiffIDs = nil

		if expected, actual := len(m.Layers), len(img.RootFS.DiffIDs); expected != actual {
			return fmt.Errorf("invalid manifest, layers length mismatch: expected %q, got %q", expected, actual)
		}

		for i, diffID := range img.RootFS.DiffIDs {
			layerPath, err := safePath(tmpDir, m.Layers[i])
			if err != nil {
				return err
			}
			r := rootFS
			r.Append(diffID)
			newLayer, err := l.ls.Get(r.ChainID())
			if err != nil {
				newLayer, err = l.loadLayer(layerPath, rootFS, diffID.String(), progressOutput)
				if err != nil {
					return err
				}
			}
			defer layer.ReleaseAndLog(l.ls, newLayer)
			if expected, actual := diffID, newLayer.DiffID(); expected != actual {
				return fmt.Errorf("invalid diffID for layer %d: expected %q, got %q", i, expected, actual)
			}
			rootFS.Append(diffID)
		}

		imgID, err := l.is.Create(config)
		if err != nil {
			return err
		}

		for _, repoTag := range m.RepoTags {
			named, err := reference.ParseNamed(repoTag)
			if err != nil {
				return err
			}
			ref, ok := named.(reference.NamedTagged)
			if !ok {
				return fmt.Errorf("invalid tag %q", repoTag)
			}
			l.setLoadedTag(ref, imgID, outStream)
			logrus.Debugf("Load() - %v(%v) has been loaded.", ref, imgID)
			sf := streamformatter.NewJSONStreamFormatter()
			outStream.Write(sf.FormatStatus("", "%v(%v) has been loaded.", ref, imgID))
		}
	}

	return nil
}

func (l *tarexporter) loadLayer(filename string, rootFS image.RootFS, id string, progressOutput progress.Output) (layer.Layer, error) {
	rawTar, err := os.Open(filename)
	if err != nil {
		logrus.Debugf("Error reading embedded tar: %v", err)
		return nil, err
	}
	defer rawTar.Close()

	inflatedLayerData, err := archive.DecompressStream(rawTar)
	if err != nil {
		return nil, err
	}
	defer inflatedLayerData.Close()

	if progressOutput != nil {
		fileInfo, err := os.Stat(filename)
		if err != nil {
			logrus.Debugf("Error statting file: %v", err)
			return nil, err
		}
		progressReader := progress.NewProgressReader(inflatedLayerData, progressOutput, fileInfo.Size(), stringid.TruncateID(id), "Loading layer")
		return l.ls.Register(progressReader, rootFS.ChainID())
	}

	return l.ls.Register(inflatedLayerData, rootFS.ChainID())
}

func (l *tarexporter) setLoadedTag(ref reference.NamedTagged, imgID image.ID, outStream io.Writer) error {
	if prevID, err := l.rs.Get(ref); err == nil && prevID != imgID {
		fmt.Fprintf(outStream, "The image %s already exists, renaming the old one with ID %s to empty string\n", ref.String(), string(prevID)) // todo: this message is wrong in case of multiple tags
	}

	if err := l.rs.AddTag(ref, imgID, true); err != nil {
		return err
	}
	return nil
}

func (l *tarexporter) ociLoad(tmpDir, name string, refs map[string]string, outStream io.Writer, progressOutput progress.Output) error {
	if name != "" && len(refs) != 0 {
		return fmt.Errorf("cannot load with either name and refs")
	}

	if name == "" && len(refs) == 0 {
		return fmt.Errorf("no OCI image name mapping provided")
	}

	var manifests []manifestItem
	indexJSON, err := os.Open(filepath.Join(tmpDir, "index.json"))
	if err != nil {
		return err
	}
	defer indexJSON.Close()
	index := ociv1.ImageIndex{}
	if err := json.NewDecoder(indexJSON).Decode(&index); err != nil {
		return err
	}
	for _, md := range index.Manifests {
		if md.MediaType != ociv1.MediaTypeImageManifest {
			continue
		}
		d := digest.Digest(md.Digest)
		manifestPath := filepath.Join(tmpDir, "blobs", d.Algorithm().String(), d.Hex())
		f, err := os.Open(manifestPath)
		if err != nil {
			return err
		}
		defer f.Close()
		man := ociv1.Manifest{}
		if err := json.NewDecoder(f).Decode(&man); err != nil {
			return err
		}
		layers := make([]string, len(man.Layers))
		for i, l := range man.Layers {
			layerDigest := digest.Digest(l.Digest)
			layers[i] = filepath.Join("blobs", layerDigest.Algorithm().String(), layerDigest.Hex())
		}
		tag := ""
		refName, ok := md.Annotations["org.opencontainers.ref.name"]
		if !ok {
			return fmt.Errorf("no ref name annotation")
		}
		if name != "" {
			named, err := reference.ParseNamed(name)
			if err != nil {
				return err
			}
			withTag, err := reference.WithTag(named, refName)
			if err != nil {
				return err
			}
			tag = withTag.String()
		} else {
			_, rs, err := getRefs(refs)
			if err != nil {
				return err
			}
			r, ok := rs[refName]
			if !ok {
				return fmt.Errorf("no naming provided for %q", refName)
			}
			tag = r.String()
		}
		configDigest := digest.Digest(man.Config.Digest)
		manifests = append(manifests, manifestItem{
			Config:   filepath.Join("blobs", configDigest.Algorithm().String(), configDigest.Hex()),
			RepoTags: []string{tag},
			Layers:   layers,
			// TODO(runcom): foreign srcs?
			// See https://github.com/docker/docker/pull/22866/files#r96125181
		})
	}

	return l.loadHelper(tmpDir, manifests, outStream, progressOutput)
}

func (l *tarexporter) legacyLoad(tmpDir string, outStream io.Writer, progressOutput progress.Output) error {
	legacyLoadedMap := make(map[string]image.ID)

	dirs, err := ioutil.ReadDir(tmpDir)
	if err != nil {
		return err
	}

	// every dir represents an image
	for _, d := range dirs {
		if d.IsDir() {
			if err := l.legacyLoadImage(d.Name(), tmpDir, legacyLoadedMap, progressOutput); err != nil {
				return err
			}
		}
	}

	// load tags from repositories file
	repositoriesPath, err := safePath(tmpDir, legacyRepositoriesFileName)
	if err != nil {
		return err
	}
	repositoriesFile, err := os.Open(repositoriesPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return err
		}
		return repositoriesFile.Close()
	}
	defer repositoriesFile.Close()

	repositories := make(map[string]map[string]string)
	if err := json.NewDecoder(repositoriesFile).Decode(&repositories); err != nil {
		return err
	}

	for name, tagMap := range repositories {
		for tag, oldID := range tagMap {
			imgID, ok := legacyLoadedMap[oldID]
			if !ok {
				return fmt.Errorf("invalid target ID: %v", oldID)
			}
			named, err := reference.WithName(name)
			if err != nil {
				return err
			}
			ref, err := reference.WithTag(named, tag)
			if err != nil {
				return err
			}
			l.setLoadedTag(ref, imgID, outStream)
			outStream.Write(streamformatter.NewJSONStreamFormatter().FormatStatus("", "%v(%v) has been loaded.", ref, imgID))
		}
	}

	return nil
}

func (l *tarexporter) legacyLoadImage(oldID, sourceDir string, loadedMap map[string]image.ID, progressOutput progress.Output) error {
	if _, loaded := loadedMap[oldID]; loaded {
		return nil
	}
	configPath, err := safePath(sourceDir, filepath.Join(oldID, legacyConfigFileName))
	if err != nil {
		return err
	}
	imageJSON, err := ioutil.ReadFile(configPath)
	if err != nil {
		logrus.Debugf("Error reading json: %v", err)
		return err
	}

	var img struct{ Parent string }
	if err := json.Unmarshal(imageJSON, &img); err != nil {
		return err
	}

	var parentID image.ID
	if img.Parent != "" {
		for {
			var loaded bool
			if parentID, loaded = loadedMap[img.Parent]; !loaded {
				if err := l.legacyLoadImage(img.Parent, sourceDir, loadedMap, progressOutput); err != nil {
					return err
				}
			} else {
				break
			}
		}
	}

	// todo: try to connect with migrate code
	rootFS := image.NewRootFS()
	var history []image.History

	if parentID != "" {
		parentImg, err := l.is.Get(parentID)
		if err != nil {
			return err
		}

		rootFS = parentImg.RootFS
		history = parentImg.History
	}

	layerPath, err := safePath(sourceDir, filepath.Join(oldID, legacyLayerFileName))
	if err != nil {
		return err
	}
	newLayer, err := l.loadLayer(layerPath, *rootFS, oldID, progressOutput)
	if err != nil {
		return err
	}
	rootFS.Append(newLayer.DiffID())

	h, err := v1.HistoryFromConfig(imageJSON, false)
	if err != nil {
		return err
	}
	history = append(history, h)

	config, err := v1.MakeConfigFromV1Config(imageJSON, rootFS, history)
	if err != nil {
		return err
	}
	imgID, err := l.is.Create(config)
	if err != nil {
		return err
	}

	metadata, err := l.ls.Release(newLayer)
	layer.LogReleaseMetadata(metadata)
	if err != nil {
		return err
	}

	if parentID != "" {
		if err := l.is.SetParent(imgID, parentID); err != nil {
			return err
		}
	}

	loadedMap[oldID] = imgID
	return nil
}

func safePath(base, path string) (string, error) {
	return symlink.FollowSymlinkInScope(filepath.Join(base, path), base)
}