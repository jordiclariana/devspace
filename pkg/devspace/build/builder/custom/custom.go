package custom

import (
	"fmt"
	"github.com/loft-sh/devspace/pkg/devspace/config/loader/variable/runtime"
	devspacecontext "github.com/loft-sh/devspace/pkg/devspace/context"
	"github.com/loft-sh/devspace/pkg/util/shell"
	"io"
	"strings"

	"github.com/bmatcuk/doublestar"
	"github.com/loft-sh/devspace/pkg/devspace/config/versions/latest"
	"github.com/loft-sh/devspace/pkg/util/command"
	"github.com/loft-sh/devspace/pkg/util/hash"
	logpkg "github.com/loft-sh/devspace/pkg/util/log"

	dockerterm "github.com/moby/term"
	"github.com/pkg/errors"
	"gopkg.in/yaml.v3"
)

var (
	_, stdout, _ = dockerterm.StdStreams()
)

// Builder holds all the relevant information for a custom build
type Builder struct {
	imageConf       *latest.Image
	imageConfigName string
	imageTags       []string
}

// NewBuilder creates a new custom builder
func NewBuilder(imageConfigName string, imageConf *latest.Image, imageTags []string) *Builder {
	return &Builder{
		imageConfigName: imageConfigName,
		imageConf:       imageConf,
		imageTags:       imageTags,
	}
}

// ShouldRebuild implements interface
func (b *Builder) ShouldRebuild(ctx *devspacecontext.Context, forceRebuild bool) (bool, error) {
	if b.imageConf.Build.Custom.OnChange == nil || len(b.imageConf.Build.Custom.OnChange) == 0 {
		return true, nil
	}

	// Hash image config
	configStr, err := yaml.Marshal(*b.imageConf)
	if err != nil {
		return false, errors.Wrap(err, "marshal image config")
	}
	imageConfigHash := hash.String(string(configStr))

	// Loop over on change globs
	customFilesHash := ""
	for _, pattern := range b.imageConf.Build.Custom.OnChange {
		files, err := doublestar.Glob(ctx.ResolvePath(pattern))
		if err != nil {
			return false, err
		}

		for _, file := range files {
			sha256, err := hash.Directory(file)
			if err != nil {
				return false, errors.Wrap(err, "hash "+file)
			}

			customFilesHash += sha256
		}
	}
	customFilesHash = hash.String(customFilesHash)

	imageCache, _ := ctx.Config.LocalCache().GetImageCache(b.imageConfigName)

	// only rebuild Docker image when Dockerfile or context has changed since latest build
	mustRebuild := forceRebuild || b.imageConf.RebuildStrategy == latest.RebuildStrategyAlways || imageCache.Tag == "" || imageCache.ImageConfigHash != imageConfigHash || imageCache.CustomFilesHash != customFilesHash

	imageCache.ImageConfigHash = imageConfigHash
	imageCache.CustomFilesHash = customFilesHash
	ctx.Config.LocalCache().SetImageCache(b.imageConfigName, imageCache)

	return mustRebuild, nil
}

// Build implements interface
func (b *Builder) Build(ctx *devspacecontext.Context) error {
	// Build arguments
	args := []string{}

	// resolve command
	if len(b.imageTags) > 0 {
		key := fmt.Sprintf("images.%s", b.imageConfigName)
		ctx.Config.SetRuntimeVariable(key, b.imageConf.Image+":"+b.imageTags[0])
		ctx.Config.SetRuntimeVariable(key+".image", b.imageConf.Image)
		ctx.Config.SetRuntimeVariable(key+".tag", b.imageTags[0])
	}

	// loop over args
	for i := range b.imageConf.Build.Custom.Args {
		resolvedArg, err := runtime.NewRuntimeResolver(ctx.WorkingDir, false).FillRuntimeVariablesAsString(b.imageConf.Build.Custom.Args[i], ctx.Config, ctx.Dependencies)
		if err != nil {
			return err
		}

		args = append(args, resolvedArg)
	}

	// add image arg
	if !b.imageConf.Build.Custom.SkipImageArg {
		for _, tag := range b.imageTags {
			if b.imageConf.Build.Custom.ImageFlag != "" {
				args = append(args, b.imageConf.Build.Custom.ImageFlag)
			}

			if !b.imageConf.Build.Custom.ImageTagOnly {
				args = append(args, b.imageConf.Image+":"+tag)
			} else {
				args = append(args, tag)
			}
		}
	}

	// append the rest
	for i := range b.imageConf.Build.Custom.AppendArgs {
		resolvedArg, err := runtime.NewRuntimeResolver(ctx.WorkingDir, false).FillRuntimeVariablesAsString(b.imageConf.Build.Custom.AppendArgs[i], ctx.Config, ctx.Dependencies)
		if err != nil {
			return err
		}

		args = append(args, resolvedArg)
	}

	// get the command
	commandPath := b.imageConf.Build.Custom.Command
	for _, c := range b.imageConf.Build.Custom.Commands {
		if !command.ShouldExecuteOnOS(c.OperatingSystem) {
			continue
		}

		commandPath = c.Command
		break
	}
	if commandPath == "" {
		return fmt.Errorf("no command specified for custom builder")
	}

	// resolve command and args
	commandPath, err := runtime.NewRuntimeResolver(ctx.WorkingDir, false).FillRuntimeVariablesAsString(commandPath, ctx.Config, ctx.Dependencies)
	if err != nil {
		return err
	}

	// Determine output writer
	var writer io.Writer
	if ctx.Log == logpkg.GetInstance() {
		writer = stdout
	} else {
		writer = ctx.Log
	}

	ctx.Log.Infof("Build %s:%s with custom command '%s %s'", b.imageConf.Image, b.imageTags[0], commandPath, strings.Join(args, " "))
	if len(args) == 0 {
		err = shell.ExecuteShellCommand(commandPath, args, ctx.WorkingDir, writer, writer, nil)
		if err != nil {
			return errors.Errorf("error building image: %v", err)
		}
	} else {
		err = command.NewStreamCommand(commandPath, args).Run(ctx.WorkingDir, writer, writer, nil)
		if err != nil {
			return errors.Errorf("error building image: %v", err)
		}
	}

	ctx.Log.Done("Done processing image '" + b.imageConf.Image + "'")
	return nil
}
