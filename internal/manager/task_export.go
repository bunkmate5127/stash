package manager

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/stashapp/stash/internal/manager/config"
	"github.com/stashapp/stash/pkg/fsutil"
	"github.com/stashapp/stash/pkg/gallery"
	"github.com/stashapp/stash/pkg/image"
	"github.com/stashapp/stash/pkg/logger"
	"github.com/stashapp/stash/pkg/models"
	"github.com/stashapp/stash/pkg/models/json"
	"github.com/stashapp/stash/pkg/models/jsonschema"
	"github.com/stashapp/stash/pkg/models/paths"
	"github.com/stashapp/stash/pkg/movie"
	"github.com/stashapp/stash/pkg/performer"
	"github.com/stashapp/stash/pkg/scene"
	"github.com/stashapp/stash/pkg/sliceutil"
	"github.com/stashapp/stash/pkg/sliceutil/stringslice"
	"github.com/stashapp/stash/pkg/studio"
	"github.com/stashapp/stash/pkg/tag"
)

type ExportTask struct {
	repository models.Repository
	full       bool

	baseDir string
	json    jsonUtils

	fileNamingAlgorithm models.HashAlgorithm

	scenes     *exportSpec
	images     *exportSpec
	performers *exportSpec
	movies     *exportSpec
	tags       *exportSpec
	studios    *exportSpec
	galleries  *exportSpec

	includeDependencies bool

	DownloadHash string
}

type ExportObjectTypeInput struct {
	Ids []string `json:"ids"`
	All *bool    `json:"all"`
}

type ExportObjectsInput struct {
	Scenes              *ExportObjectTypeInput `json:"scenes"`
	Images              *ExportObjectTypeInput `json:"images"`
	Studios             *ExportObjectTypeInput `json:"studios"`
	Performers          *ExportObjectTypeInput `json:"performers"`
	Tags                *ExportObjectTypeInput `json:"tags"`
	Movies              *ExportObjectTypeInput `json:"movies"`
	Galleries           *ExportObjectTypeInput `json:"galleries"`
	IncludeDependencies *bool                  `json:"includeDependencies"`
}

type exportSpec struct {
	IDs []int
	all bool
}

func newExportSpec(input *ExportObjectTypeInput) *exportSpec {
	if input == nil {
		return &exportSpec{}
	}

	ids, _ := stringslice.StringSliceToIntSlice(input.Ids)

	ret := &exportSpec{
		IDs: ids,
	}

	if input.All != nil {
		ret.all = *input.All
	}

	return ret
}

func CreateExportTask(a models.HashAlgorithm, input ExportObjectsInput) *ExportTask {
	includeDeps := false
	if input.IncludeDependencies != nil {
		includeDeps = *input.IncludeDependencies
	}

	return &ExportTask{
		repository:          GetInstance().Repository,
		fileNamingAlgorithm: a,
		scenes:              newExportSpec(input.Scenes),
		images:              newExportSpec(input.Images),
		performers:          newExportSpec(input.Performers),
		movies:              newExportSpec(input.Movies),
		tags:                newExportSpec(input.Tags),
		studios:             newExportSpec(input.Studios),
		galleries:           newExportSpec(input.Galleries),
		includeDependencies: includeDeps,
	}
}

func (t *ExportTask) Start(ctx context.Context, wg *sync.WaitGroup) {
	defer wg.Done()
	// @manager.total = Scene.count + Gallery.count + Performer.count + Studio.count + Movie.count
	workerCount := runtime.GOMAXPROCS(0) // set worker count to number of cpus available

	startTime := time.Now()

	if t.full {
		t.baseDir = config.GetInstance().GetMetadataPath()
	} else {
		var err error
		t.baseDir, err = instance.Paths.Generated.TempDir("export")
		if err != nil {
			logger.Errorf("error creating temporary directory for export: %s", err.Error())
			return
		}

		defer func() {
			err := fsutil.RemoveDir(t.baseDir)
			if err != nil {
				logger.Errorf("error removing directory %s: %s", t.baseDir, err.Error())
			}
		}()
	}

	if t.baseDir == "" {
		logger.Errorf("baseDir must not be empty")
		return
	}

	t.json = jsonUtils{
		json: *paths.GetJSONPaths(t.baseDir),
	}

	paths.EmptyJSONDirs(t.baseDir)
	paths.EnsureJSONDirs(t.baseDir)

	txnErr := t.repository.WithTxn(ctx, func(ctx context.Context) error {
		// include movie scenes and gallery images
		if !t.full {
			// only include movie scenes if includeDependencies is also set
			if !t.scenes.all && t.includeDependencies {
				t.populateMovieScenes(ctx)
			}

			// always export gallery images
			if !t.images.all {
				t.populateGalleryImages(ctx)
			}
		}

		t.ExportScenes(ctx, workerCount)
		t.ExportImages(ctx, workerCount)
		t.ExportGalleries(ctx, workerCount)
		t.ExportMovies(ctx, workerCount)
		t.ExportPerformers(ctx, workerCount)
		t.ExportStudios(ctx, workerCount)
		t.ExportTags(ctx, workerCount)

		return nil
	})
	if txnErr != nil {
		logger.Warnf("error while running export transaction: %v", txnErr)
	}

	if !t.full {
		err := t.generateDownload()
		if err != nil {
			logger.Errorf("error generating download link: %s", err.Error())
			return
		}
	}
	logger.Infof("Export complete in %s.", time.Since(startTime))
}

func (t *ExportTask) generateDownload() error {
	// zip the files and register a download link
	if err := fsutil.EnsureDir(instance.Paths.Generated.Downloads); err != nil {
		return err
	}
	z, err := os.CreateTemp(instance.Paths.Generated.Downloads, "export*.zip")
	if err != nil {
		return err
	}
	defer z.Close()

	err = t.zipFiles(z)
	if err != nil {
		return err
	}

	t.DownloadHash, err = instance.DownloadStore.RegisterFile(z.Name(), "", false)
	if err != nil {
		return fmt.Errorf("error registering file for download: %w", err)
	}
	logger.Debugf("Generated zip file %s with hash %s", z.Name(), t.DownloadHash)
	return nil
}

func (t *ExportTask) zipFiles(w io.Writer) error {
	z := zip.NewWriter(w)
	defer z.Close()

	u := jsonUtils{
		json: *paths.GetJSONPaths(""),
	}

	walkWarn(t.json.json.Tags, t.zipWalkFunc(u.json.Tags, z))
	walkWarn(t.json.json.Galleries, t.zipWalkFunc(u.json.Galleries, z))
	walkWarn(t.json.json.Performers, t.zipWalkFunc(u.json.Performers, z))
	walkWarn(t.json.json.Studios, t.zipWalkFunc(u.json.Studios, z))
	walkWarn(t.json.json.Movies, t.zipWalkFunc(u.json.Movies, z))
	walkWarn(t.json.json.Scenes, t.zipWalkFunc(u.json.Scenes, z))
	walkWarn(t.json.json.Images, t.zipWalkFunc(u.json.Images, z))

	return nil
}

// like filepath.Walk but issue a warning on error
func walkWarn(root string, fn filepath.WalkFunc) {
	if err := filepath.Walk(root, fn); err != nil {
		logger.Warnf("error walking structure %v: %v", root, err)
	}
}

func (t *ExportTask) zipWalkFunc(outDir string, z *zip.Writer) filepath.WalkFunc {
	return func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		return t.zipFile(path, outDir, z)
	}
}

func (t *ExportTask) zipFile(fn, outDir string, z *zip.Writer) error {
	bn := filepath.Base(fn)

	p := filepath.Join(outDir, bn)
	p = filepath.ToSlash(p)

	f, err := z.Create(p)
	if err != nil {
		return fmt.Errorf("error creating zip entry for %s: %s", fn, err.Error())
	}

	i, err := os.Open(fn)
	if err != nil {
		return fmt.Errorf("error opening %s: %s", fn, err.Error())
	}

	defer i.Close()

	if _, err := io.Copy(f, i); err != nil {
		return fmt.Errorf("error writing %s to zip: %s", fn, err.Error())
	}

	return nil
}

func (t *ExportTask) populateMovieScenes(ctx context.Context) {
	r := t.repository
	reader := r.Movie
	sceneReader := r.Scene

	var movies []*models.Movie
	var err error
	all := t.full || (t.movies != nil && t.movies.all)
	if all {
		movies, err = reader.All(ctx)
	} else if t.movies != nil && len(t.movies.IDs) > 0 {
		movies, err = reader.FindMany(ctx, t.movies.IDs)
	}

	if err != nil {
		logger.Errorf("[movies] failed to fetch movies: %v", err)
	}

	for _, m := range movies {
		scenes, err := sceneReader.FindByMovieID(ctx, m.ID)
		if err != nil {
			logger.Errorf("[movies] <%s> failed to fetch scenes for movie: %v", m.Name, err)
			continue
		}

		for _, s := range scenes {
			t.scenes.IDs = sliceutil.AppendUnique(t.scenes.IDs, s.ID)
		}
	}
}

func (t *ExportTask) populateGalleryImages(ctx context.Context) {
	r := t.repository
	reader := r.Gallery
	imageReader := r.Image

	var galleries []*models.Gallery
	var err error
	all := t.full || (t.galleries != nil && t.galleries.all)
	if all {
		galleries, err = reader.All(ctx)
	} else if t.galleries != nil && len(t.galleries.IDs) > 0 {
		galleries, err = reader.FindMany(ctx, t.galleries.IDs)
	}

	if err != nil {
		logger.Errorf("[galleries] failed to fetch galleries: %s", err.Error())
	}

	for _, g := range galleries {
		if err := g.LoadFiles(ctx, reader); err != nil {
			logger.Errorf("[galleries] <%s> failed to fetch files for gallery: %s", g.DisplayName(), err.Error())
			continue
		}

		images, err := imageReader.FindByGalleryID(ctx, g.ID)
		if err != nil {
			logger.Errorf("[galleries] <%s> failed to fetch images for gallery: %s", g.PrimaryChecksum(), err.Error())
			continue
		}

		for _, i := range images {
			t.images.IDs = sliceutil.AppendUnique(t.images.IDs, i.ID)
		}
	}
}

func (t *ExportTask) ExportScenes(ctx context.Context, workers int) {
	var scenesWg sync.WaitGroup

	sceneReader := t.repository.Scene

	var scenes []*models.Scene
	var err error
	all := t.full || (t.scenes != nil && t.scenes.all)
	if all {
		scenes, err = sceneReader.All(ctx)
	} else if t.scenes != nil && len(t.scenes.IDs) > 0 {
		scenes, err = sceneReader.FindMany(ctx, t.scenes.IDs)
	}

	if err != nil {
		logger.Errorf("[scenes] failed to fetch scenes: %s", err.Error())
	}

	jobCh := make(chan *models.Scene, workers*2) // make a buffered channel to feed workers

	logger.Info("[scenes] exporting")
	startTime := time.Now()

	for w := 0; w < workers; w++ { // create export Scene workers
		scenesWg.Add(1)
		go t.exportScene(ctx, &scenesWg, jobCh)
	}

	for i, scene := range scenes {
		index := i + 1

		if (i % 100) == 0 { // make progress easier to read
			logger.Progressf("[scenes] %d of %d", index, len(scenes))
		}
		jobCh <- scene // feed workers
	}

	close(jobCh) // close channel so that workers will know no more jobs are available
	scenesWg.Wait()

	logger.Infof("[scenes] export complete in %s. %d workers used.", time.Since(startTime), workers)
}

func (t *ExportTask) exportFile(f models.File) {
	newFileJSON := fileToJSON(f)

	fn := newFileJSON.Filename()

	if err := t.json.saveFile(fn, newFileJSON); err != nil {
		logger.Errorf("[files] <%s> failed to save json: %s", fn, err.Error())
	}
}

func fileToJSON(f models.File) jsonschema.DirEntry {
	bf := f.Base()

	base := jsonschema.BaseFile{
		BaseDirEntry: jsonschema.BaseDirEntry{
			Type:      jsonschema.DirEntryTypeFile,
			ModTime:   json.JSONTime{Time: bf.ModTime},
			Path:      bf.Path,
			CreatedAt: json.JSONTime{Time: bf.CreatedAt},
			UpdatedAt: json.JSONTime{Time: bf.UpdatedAt},
		},
		Size: bf.Size,
	}

	if bf.ZipFile != nil {
		base.ZipFile = bf.ZipFile.Base().Path
	}

	for _, fp := range bf.Fingerprints {
		base.Fingerprints = append(base.Fingerprints, jsonschema.Fingerprint{
			Type:        fp.Type,
			Fingerprint: fp.Fingerprint,
		})
	}

	switch ff := f.(type) {
	case *models.VideoFile:
		base.Type = jsonschema.DirEntryTypeVideo
		return jsonschema.VideoFile{
			BaseFile:         &base,
			Format:           ff.Format,
			Width:            ff.Width,
			Height:           ff.Height,
			Duration:         ff.Duration,
			VideoCodec:       ff.VideoCodec,
			AudioCodec:       ff.AudioCodec,
			FrameRate:        ff.FrameRate,
			BitRate:          ff.BitRate,
			Interactive:      ff.Interactive,
			InteractiveSpeed: ff.InteractiveSpeed,
		}
	case *models.ImageFile:
		base.Type = jsonschema.DirEntryTypeImage
		return jsonschema.ImageFile{
			BaseFile: &base,
			Format:   ff.Format,
			Width:    ff.Width,
			Height:   ff.Height,
		}
	}

	return &base
}

func (t *ExportTask) exportFolder(f models.Folder) {
	newFileJSON := folderToJSON(f)

	fn := newFileJSON.Filename()

	if err := t.json.saveFile(fn, newFileJSON); err != nil {
		logger.Errorf("[files] <%s> failed to save json: %s", fn, err.Error())
	}
}

func folderToJSON(f models.Folder) jsonschema.DirEntry {
	base := jsonschema.BaseDirEntry{
		Type:      jsonschema.DirEntryTypeFolder,
		ModTime:   json.JSONTime{Time: f.ModTime},
		Path:      f.Path,
		CreatedAt: json.JSONTime{Time: f.CreatedAt},
		UpdatedAt: json.JSONTime{Time: f.UpdatedAt},
	}

	if f.ZipFile != nil {
		base.ZipFile = f.ZipFile.Base().Path
	}

	return &base
}

func (t *ExportTask) exportScene(ctx context.Context, wg *sync.WaitGroup, jobChan <-chan *models.Scene) {
	defer wg.Done()

	r := t.repository
	sceneReader := r.Scene
	studioReader := r.Studio
	movieReader := r.Movie
	galleryReader := r.Gallery
	performerReader := r.Performer
	tagReader := r.Tag
	sceneMarkerReader := r.SceneMarker

	for s := range jobChan {
		sceneHash := s.GetHash(t.fileNamingAlgorithm)

		if err := s.LoadRelationships(ctx, sceneReader); err != nil {
			logger.Errorf("[scenes] <%s> error loading scene relationships: %v", sceneHash, err)
		}

		newSceneJSON, err := scene.ToBasicJSON(ctx, sceneReader, s)
		if err != nil {
			logger.Errorf("[scenes] <%s> error getting scene JSON: %s", sceneHash, err.Error())
			continue
		}

		// export files
		for _, f := range s.Files.List() {
			t.exportFile(f)
		}

		newSceneJSON.Studio, err = scene.GetStudioName(ctx, studioReader, s)
		if err != nil {
			logger.Errorf("[scenes] <%s> error getting scene studio name: %s", sceneHash, err.Error())
			continue
		}

		galleries, err := galleryReader.FindBySceneID(ctx, s.ID)
		if err != nil {
			logger.Errorf("[scenes] <%s> error getting scene gallery checksums: %s", sceneHash, err.Error())
			continue
		}

		for _, g := range galleries {
			if err := g.LoadFiles(ctx, galleryReader); err != nil {
				logger.Errorf("[scenes] <%s> error getting scene gallery files: %s", sceneHash, err.Error())
				continue
			}
		}

		newSceneJSON.Galleries = gallery.GetRefs(galleries)

		newSceneJSON.ResumeTime = s.ResumeTime
		newSceneJSON.PlayCount = s.PlayCount
		newSceneJSON.PlayDuration = s.PlayDuration

		performers, err := performerReader.FindBySceneID(ctx, s.ID)
		if err != nil {
			logger.Errorf("[scenes] <%s> error getting scene performer names: %s", sceneHash, err.Error())
			continue
		}

		newSceneJSON.Performers = performer.GetNames(performers)

		newSceneJSON.Tags, err = scene.GetTagNames(ctx, tagReader, s)
		if err != nil {
			logger.Errorf("[scenes] <%s> error getting scene tag names: %s", sceneHash, err.Error())
			continue
		}

		newSceneJSON.Markers, err = scene.GetSceneMarkersJSON(ctx, sceneMarkerReader, tagReader, s)
		if err != nil {
			logger.Errorf("[scenes] <%s> error getting scene markers JSON: %s", sceneHash, err.Error())
			continue
		}

		newSceneJSON.Movies, err = scene.GetSceneMoviesJSON(ctx, movieReader, s)
		if err != nil {
			logger.Errorf("[scenes] <%s> error getting scene movies JSON: %s", sceneHash, err.Error())
			continue
		}

		if t.includeDependencies {
			if s.StudioID != nil {
				t.studios.IDs = sliceutil.AppendUnique(t.studios.IDs, *s.StudioID)
			}

			t.galleries.IDs = sliceutil.AppendUniques(t.galleries.IDs, gallery.GetIDs(galleries))

			tagIDs, err := scene.GetDependentTagIDs(ctx, tagReader, sceneMarkerReader, s)
			if err != nil {
				logger.Errorf("[scenes] <%s> error getting scene tags: %s", sceneHash, err.Error())
				continue
			}
			t.tags.IDs = sliceutil.AppendUniques(t.tags.IDs, tagIDs)

			movieIDs, err := scene.GetDependentMovieIDs(ctx, s)
			if err != nil {
				logger.Errorf("[scenes] <%s> error getting scene movies: %s", sceneHash, err.Error())
				continue
			}
			t.movies.IDs = sliceutil.AppendUniques(t.movies.IDs, movieIDs)

			t.performers.IDs = sliceutil.AppendUniques(t.performers.IDs, performer.GetIDs(performers))
		}

		basename := filepath.Base(s.Path)
		hash := s.OSHash

		fn := newSceneJSON.Filename(s.ID, basename, hash)

		if err := t.json.saveScene(fn, newSceneJSON); err != nil {
			logger.Errorf("[scenes] <%s> failed to save json: %s", sceneHash, err.Error())
		}
	}
}

func (t *ExportTask) ExportImages(ctx context.Context, workers int) {
	var imagesWg sync.WaitGroup

	r := t.repository
	imageReader := r.Image

	var images []*models.Image
	var err error
	all := t.full || (t.images != nil && t.images.all)
	if all {
		images, err = imageReader.All(ctx)
	} else if t.images != nil && len(t.images.IDs) > 0 {
		images, err = imageReader.FindMany(ctx, t.images.IDs)
	}

	if err != nil {
		logger.Errorf("[images] failed to fetch images: %s", err.Error())
	}

	jobCh := make(chan *models.Image, workers*2) // make a buffered channel to feed workers

	logger.Info("[images] exporting")
	startTime := time.Now()

	for w := 0; w < workers; w++ { // create export Image workers
		imagesWg.Add(1)
		go t.exportImage(ctx, &imagesWg, jobCh)
	}

	for i, image := range images {
		index := i + 1

		if (i % 100) == 0 { // make progress easier to read
			logger.Progressf("[images] %d of %d", index, len(images))
		}
		jobCh <- image // feed workers
	}

	close(jobCh) // close channel so that workers will know no more jobs are available
	imagesWg.Wait()

	logger.Infof("[images] export complete in %s. %d workers used.", time.Since(startTime), workers)
}

func (t *ExportTask) exportImage(ctx context.Context, wg *sync.WaitGroup, jobChan <-chan *models.Image) {
	defer wg.Done()

	r := t.repository
	studioReader := r.Studio
	galleryReader := r.Gallery
	performerReader := r.Performer
	tagReader := r.Tag

	for s := range jobChan {
		imageHash := s.Checksum

		if err := s.LoadFiles(ctx, r.Image); err != nil {
			logger.Errorf("[images] <%s> error getting image files: %s", imageHash, err.Error())
			continue
		}

		if err := s.LoadURLs(ctx, r.Image); err != nil {
			logger.Errorf("[images] <%s> error getting image urls: %s", imageHash, err.Error())
			continue
		}

		newImageJSON := image.ToBasicJSON(s)

		// export files
		for _, f := range s.Files.List() {
			t.exportFile(f)
		}

		var err error
		newImageJSON.Studio, err = image.GetStudioName(ctx, studioReader, s)
		if err != nil {
			logger.Errorf("[images] <%s> error getting image studio name: %s", imageHash, err.Error())
			continue
		}

		imageGalleries, err := galleryReader.FindByImageID(ctx, s.ID)
		if err != nil {
			logger.Errorf("[images] <%s> error getting image galleries: %s", imageHash, err.Error())
			continue
		}

		for _, g := range imageGalleries {
			if err := g.LoadFiles(ctx, galleryReader); err != nil {
				logger.Errorf("[images] <%s> error getting image gallery files: %s", imageHash, err.Error())
				continue
			}
		}

		newImageJSON.Galleries = gallery.GetRefs(imageGalleries)

		performers, err := performerReader.FindByImageID(ctx, s.ID)
		if err != nil {
			logger.Errorf("[images] <%s> error getting image performer names: %s", imageHash, err.Error())
			continue
		}

		newImageJSON.Performers = performer.GetNames(performers)

		tags, err := tagReader.FindByImageID(ctx, s.ID)
		if err != nil {
			logger.Errorf("[images] <%s> error getting image tag names: %s", imageHash, err.Error())
			continue
		}

		newImageJSON.Tags = tag.GetNames(tags)

		if t.includeDependencies {
			if s.StudioID != nil {
				t.studios.IDs = sliceutil.AppendUnique(t.studios.IDs, *s.StudioID)
			}

			t.galleries.IDs = sliceutil.AppendUniques(t.galleries.IDs, gallery.GetIDs(imageGalleries))
			t.tags.IDs = sliceutil.AppendUniques(t.tags.IDs, tag.GetIDs(tags))
			t.performers.IDs = sliceutil.AppendUniques(t.performers.IDs, performer.GetIDs(performers))
		}

		fn := newImageJSON.Filename(filepath.Base(s.Path), s.Checksum)

		if err := t.json.saveImage(fn, newImageJSON); err != nil {
			logger.Errorf("[images] <%s> failed to save json: %s", imageHash, err.Error())
		}
	}
}

func (t *ExportTask) ExportGalleries(ctx context.Context, workers int) {
	var galleriesWg sync.WaitGroup

	reader := t.repository.Gallery

	var galleries []*models.Gallery
	var err error
	all := t.full || (t.galleries != nil && t.galleries.all)
	if all {
		galleries, err = reader.All(ctx)
	} else if t.galleries != nil && len(t.galleries.IDs) > 0 {
		galleries, err = reader.FindMany(ctx, t.galleries.IDs)
	}

	if err != nil {
		logger.Errorf("[galleries] failed to fetch galleries: %s", err.Error())
	}

	jobCh := make(chan *models.Gallery, workers*2) // make a buffered channel to feed workers

	logger.Info("[galleries] exporting")
	startTime := time.Now()

	for w := 0; w < workers; w++ { // create export Scene workers
		galleriesWg.Add(1)
		go t.exportGallery(ctx, &galleriesWg, jobCh)
	}

	for i, gallery := range galleries {
		index := i + 1

		if (i % 100) == 0 { // make progress easier to read
			logger.Progressf("[galleries] %d of %d", index, len(galleries))
		}

		jobCh <- gallery
	}

	close(jobCh) // close channel so that workers will know no more jobs are available
	galleriesWg.Wait()

	logger.Infof("[galleries] export complete in %s. %d workers used.", time.Since(startTime), workers)
}

func (t *ExportTask) exportGallery(ctx context.Context, wg *sync.WaitGroup, jobChan <-chan *models.Gallery) {
	defer wg.Done()

	r := t.repository
	studioReader := r.Studio
	performerReader := r.Performer
	tagReader := r.Tag
	galleryChapterReader := r.GalleryChapter

	for g := range jobChan {
		if err := g.LoadFiles(ctx, r.Gallery); err != nil {
			logger.Errorf("[galleries] <%s> failed to fetch files for gallery: %s", g.DisplayName(), err.Error())
			continue
		}

		galleryHash := g.PrimaryChecksum()

		newGalleryJSON, err := gallery.ToBasicJSON(g)
		if err != nil {
			logger.Errorf("[galleries] <%s> error getting gallery JSON: %s", galleryHash, err.Error())
			continue
		}

		// export files
		for _, f := range g.Files.List() {
			t.exportFile(f)
		}

		// export folder if necessary
		if g.FolderID != nil {
			folder, err := r.Folder.Find(ctx, *g.FolderID)
			if err != nil {
				logger.Errorf("[galleries] <%s> error getting gallery folder: %v", galleryHash, err)
				continue
			}

			if folder == nil {
				logger.Errorf("[galleries] <%s> unable to find gallery folder", galleryHash)
				continue
			}

			t.exportFolder(*folder)
		}

		newGalleryJSON.Studio, err = gallery.GetStudioName(ctx, studioReader, g)
		if err != nil {
			logger.Errorf("[galleries] <%s> error getting gallery studio name: %s", galleryHash, err.Error())
			continue
		}

		performers, err := performerReader.FindByGalleryID(ctx, g.ID)
		if err != nil {
			logger.Errorf("[galleries] <%s> error getting gallery performer names: %s", galleryHash, err.Error())
			continue
		}

		newGalleryJSON.Performers = performer.GetNames(performers)

		tags, err := tagReader.FindByGalleryID(ctx, g.ID)
		if err != nil {
			logger.Errorf("[galleries] <%s> error getting gallery tag names: %s", galleryHash, err.Error())
			continue
		}

		newGalleryJSON.Chapters, err = gallery.GetGalleryChaptersJSON(ctx, galleryChapterReader, g)
		if err != nil {
			logger.Errorf("[galleries] <%s> error getting gallery chapters JSON: %s", galleryHash, err.Error())
			continue
		}

		newGalleryJSON.Tags = tag.GetNames(tags)

		if t.includeDependencies {
			if g.StudioID != nil {
				t.studios.IDs = sliceutil.AppendUnique(t.studios.IDs, *g.StudioID)
			}

			t.tags.IDs = sliceutil.AppendUniques(t.tags.IDs, tag.GetIDs(tags))
			t.performers.IDs = sliceutil.AppendUniques(t.performers.IDs, performer.GetIDs(performers))
		}

		basename := ""
		// use id in case multiple galleries with the same basename
		hash := strconv.Itoa(g.ID)

		switch {
		case g.Path != "":
			basename = filepath.Base(g.Path)
		default:
			basename = g.Title
		}

		fn := newGalleryJSON.Filename(basename, hash)

		if err := t.json.saveGallery(fn, newGalleryJSON); err != nil {
			logger.Errorf("[galleries] <%s> failed to save json: %s", galleryHash, err.Error())
		}
	}
}

func (t *ExportTask) ExportPerformers(ctx context.Context, workers int) {
	var performersWg sync.WaitGroup

	reader := t.repository.Performer
	var performers []*models.Performer
	var err error
	all := t.full || (t.performers != nil && t.performers.all)
	if all {
		performers, err = reader.All(ctx)
	} else if t.performers != nil && len(t.performers.IDs) > 0 {
		performers, err = reader.FindMany(ctx, t.performers.IDs)
	}

	if err != nil {
		logger.Errorf("[performers] failed to fetch performers: %s", err.Error())
	}
	jobCh := make(chan *models.Performer, workers*2) // make a buffered channel to feed workers

	logger.Info("[performers] exporting")
	startTime := time.Now()

	for w := 0; w < workers; w++ { // create export Performer workers
		performersWg.Add(1)
		go t.exportPerformer(ctx, &performersWg, jobCh)
	}

	for i, performer := range performers {
		index := i + 1
		logger.Progressf("[performers] %d of %d", index, len(performers))

		jobCh <- performer // feed workers
	}

	close(jobCh) // close channel so workers will know that no more jobs are available
	performersWg.Wait()

	logger.Infof("[performers] export complete in %s. %d workers used.", time.Since(startTime), workers)
}

func (t *ExportTask) exportPerformer(ctx context.Context, wg *sync.WaitGroup, jobChan <-chan *models.Performer) {
	defer wg.Done()

	r := t.repository
	performerReader := r.Performer

	for p := range jobChan {
		newPerformerJSON, err := performer.ToJSON(ctx, performerReader, p)

		if err != nil {
			logger.Errorf("[performers] <%s> error getting performer JSON: %s", p.Name, err.Error())
			continue
		}

		tags, err := r.Tag.FindByPerformerID(ctx, p.ID)
		if err != nil {
			logger.Errorf("[performers] <%s> error getting performer tags: %s", p.Name, err.Error())
			continue
		}

		newPerformerJSON.Tags = tag.GetNames(tags)

		if t.includeDependencies {
			t.tags.IDs = sliceutil.AppendUniques(t.tags.IDs, tag.GetIDs(tags))
		}

		fn := newPerformerJSON.Filename()

		if err := t.json.savePerformer(fn, newPerformerJSON); err != nil {
			logger.Errorf("[performers] <%s> failed to save json: %s", p.Name, err.Error())
		}
	}
}

func (t *ExportTask) ExportStudios(ctx context.Context, workers int) {
	var studiosWg sync.WaitGroup

	reader := t.repository.Studio
	var studios []*models.Studio
	var err error
	all := t.full || (t.studios != nil && t.studios.all)
	if all {
		studios, err = reader.All(ctx)
	} else if t.studios != nil && len(t.studios.IDs) > 0 {
		studios, err = reader.FindMany(ctx, t.studios.IDs)
	}

	if err != nil {
		logger.Errorf("[studios] failed to fetch studios: %s", err.Error())
	}

	logger.Info("[studios] exporting")
	startTime := time.Now()

	jobCh := make(chan *models.Studio, workers*2) // make a buffered channel to feed workers

	for w := 0; w < workers; w++ { // create export Studio workers
		studiosWg.Add(1)
		go t.exportStudio(ctx, &studiosWg, jobCh)
	}

	for i, studio := range studios {
		index := i + 1
		logger.Progressf("[studios] %d of %d", index, len(studios))

		jobCh <- studio // feed workers
	}

	close(jobCh)
	studiosWg.Wait()

	logger.Infof("[studios] export complete in %s. %d workers used.", time.Since(startTime), workers)
}

func (t *ExportTask) exportStudio(ctx context.Context, wg *sync.WaitGroup, jobChan <-chan *models.Studio) {
	defer wg.Done()

	studioReader := t.repository.Studio

	for s := range jobChan {
		newStudioJSON, err := studio.ToJSON(ctx, studioReader, s)

		if err != nil {
			logger.Errorf("[studios] <%s> error getting studio JSON: %v", s.Name, err)
			continue
		}

		fn := newStudioJSON.Filename()

		if err := t.json.saveStudio(fn, newStudioJSON); err != nil {
			logger.Errorf("[studios] <%s> failed to save json: %v", s.Name, err)
		}
	}
}

func (t *ExportTask) ExportTags(ctx context.Context, workers int) {
	var tagsWg sync.WaitGroup

	reader := t.repository.Tag
	var tags []*models.Tag
	var err error
	all := t.full || (t.tags != nil && t.tags.all)
	if all {
		tags, err = reader.All(ctx)
	} else if t.tags != nil && len(t.tags.IDs) > 0 {
		tags, err = reader.FindMany(ctx, t.tags.IDs)
	}

	if err != nil {
		logger.Errorf("[tags] failed to fetch tags: %s", err.Error())
	}

	logger.Info("[tags] exporting")
	startTime := time.Now()

	jobCh := make(chan *models.Tag, workers*2) // make a buffered channel to feed workers

	for w := 0; w < workers; w++ { // create export Tag workers
		tagsWg.Add(1)
		go t.exportTag(ctx, &tagsWg, jobCh)
	}

	for i, tag := range tags {
		index := i + 1
		logger.Progressf("[tags] %d of %d", index, len(tags))

		jobCh <- tag // feed workers
	}

	close(jobCh)
	tagsWg.Wait()

	logger.Infof("[tags] export complete in %s. %d workers used.", time.Since(startTime), workers)
}

func (t *ExportTask) exportTag(ctx context.Context, wg *sync.WaitGroup, jobChan <-chan *models.Tag) {
	defer wg.Done()

	tagReader := t.repository.Tag

	for thisTag := range jobChan {
		newTagJSON, err := tag.ToJSON(ctx, tagReader, thisTag)

		if err != nil {
			logger.Errorf("[tags] <%s> error getting tag JSON: %s", thisTag.Name, err.Error())
			continue
		}

		fn := newTagJSON.Filename()

		if err := t.json.saveTag(fn, newTagJSON); err != nil {
			logger.Errorf("[tags] <%s> failed to save json: %s", fn, err.Error())
		}
	}
}

func (t *ExportTask) ExportMovies(ctx context.Context, workers int) {
	var moviesWg sync.WaitGroup

	reader := t.repository.Movie
	var movies []*models.Movie
	var err error
	all := t.full || (t.movies != nil && t.movies.all)
	if all {
		movies, err = reader.All(ctx)
	} else if t.movies != nil && len(t.movies.IDs) > 0 {
		movies, err = reader.FindMany(ctx, t.movies.IDs)
	}

	if err != nil {
		logger.Errorf("[movies] failed to fetch movies: %s", err.Error())
	}

	logger.Info("[movies] exporting")
	startTime := time.Now()

	jobCh := make(chan *models.Movie, workers*2) // make a buffered channel to feed workers

	for w := 0; w < workers; w++ { // create export Studio workers
		moviesWg.Add(1)
		go t.exportMovie(ctx, &moviesWg, jobCh)
	}

	for i, movie := range movies {
		index := i + 1
		logger.Progressf("[movies] %d of %d", index, len(movies))

		jobCh <- movie // feed workers
	}

	close(jobCh)
	moviesWg.Wait()

	logger.Infof("[movies] export complete in %s. %d workers used.", time.Since(startTime), workers)

}
func (t *ExportTask) exportMovie(ctx context.Context, wg *sync.WaitGroup, jobChan <-chan *models.Movie) {
	defer wg.Done()

	r := t.repository
	movieReader := r.Movie
	studioReader := r.Studio

	for m := range jobChan {
		newMovieJSON, err := movie.ToJSON(ctx, movieReader, studioReader, m)

		if err != nil {
			logger.Errorf("[movies] <%s> error getting tag JSON: %v", m.Name, err)
			continue
		}

		if t.includeDependencies {
			if m.StudioID != nil {
				t.studios.IDs = sliceutil.AppendUnique(t.studios.IDs, *m.StudioID)
			}
		}

		fn := newMovieJSON.Filename()

		if err := t.json.saveMovie(fn, newMovieJSON); err != nil {
			logger.Errorf("[movies] <%s> failed to save json: %v", m.Name, err)
		}
	}
}
