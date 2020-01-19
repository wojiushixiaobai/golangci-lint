package lint

import (
	"context"
	"fmt"
	"os"
	"runtime/debug"
	"strings"

	"github.com/golangci/golangci-lint/internal/errorutil"
	"github.com/golangci/golangci-lint/pkg/config"
	"github.com/golangci/golangci-lint/pkg/fsutils"
	"github.com/golangci/golangci-lint/pkg/goutil"
	"github.com/golangci/golangci-lint/pkg/lint/linter"
	"github.com/golangci/golangci-lint/pkg/lint/lintersdb"
	"github.com/golangci/golangci-lint/pkg/logutils"
	"github.com/golangci/golangci-lint/pkg/packages"
	"github.com/golangci/golangci-lint/pkg/result"
	"github.com/golangci/golangci-lint/pkg/result/processors"
	"github.com/golangci/golangci-lint/pkg/timeutils"

	gopackages "golang.org/x/tools/go/packages"
)

type Runner struct {
	Processors []processors.Processor
	Log        logutils.Log
}

func NewRunner(cfg *config.Config, log logutils.Log, goenv *goutil.Env,
	lineCache *fsutils.LineCache, dbManager *lintersdb.Manager, pkgs []*gopackages.Package) (*Runner, error) {
	icfg := cfg.Issues
	excludePatterns := icfg.ExcludePatterns
	if icfg.UseDefaultExcludes {
		excludePatterns = append(excludePatterns, config.GetDefaultExcludePatternsStrings()...)
	}

	var excludeTotalPattern string
	if len(excludePatterns) != 0 {
		excludeTotalPattern = fmt.Sprintf("(%s)", strings.Join(excludePatterns, "|"))
	}

	skipFilesProcessor, err := processors.NewSkipFiles(cfg.Run.SkipFiles)
	if err != nil {
		return nil, err
	}

	skipDirs := cfg.Run.SkipDirs
	if cfg.Run.UseDefaultSkipDirs {
		skipDirs = append(skipDirs, packages.StdExcludeDirRegexps...)
	}
	skipDirsProcessor, err := processors.NewSkipDirs(skipDirs, log.Child("skip dirs"), cfg.Run.Args)
	if err != nil {
		return nil, err
	}

	var excludeRules []processors.ExcludeRule
	for _, r := range icfg.ExcludeRules {
		excludeRules = append(excludeRules, processors.ExcludeRule{
			Text:    r.Text,
			Source:  r.Source,
			Path:    r.Path,
			Linters: r.Linters,
		})
	}

	return &Runner{
		Processors: []processors.Processor{
			processors.NewCgo(goenv),

			// Must go after Cgo.
			processors.NewFilenameUnadjuster(pkgs, log.Child("filename_unadjuster")),

			// Must be before diff, nolint and exclude autogenerated processor at least.
			processors.NewPathPrettifier(),
			skipFilesProcessor,
			skipDirsProcessor, // must be after path prettifier

			processors.NewAutogeneratedExclude(),

			// Must be before exclude because users see already marked output and configure excluding by it.
			processors.NewIdentifierMarker(),

			processors.NewExclude(excludeTotalPattern),
			processors.NewExcludeRules(excludeRules, lineCache, log.Child("exclude_rules")),
			processors.NewNolint(log.Child("nolint"), dbManager),

			processors.NewUniqByLine(cfg),
			processors.NewDiff(icfg.Diff, icfg.DiffFromRevision, icfg.DiffPatchFilePath),
			processors.NewMaxPerFileFromLinter(cfg),
			processors.NewMaxSameIssues(icfg.MaxSameIssues, log.Child("max_same_issues"), cfg),
			processors.NewMaxFromLinter(icfg.MaxIssuesPerLinter, log.Child("max_from_linter"), cfg),
			processors.NewSourceCode(lineCache, log.Child("source_code")),
			processors.NewPathShortener(),
		},
		Log: log,
	}, nil
}

func (r *Runner) runLinterSafe(ctx context.Context, lintCtx *linter.Context,
	lc *linter.Config) (ret []result.Issue, err error) {
	defer func() {
		if panicData := recover(); panicData != nil {
			if pe, ok := panicData.(*errorutil.PanicError); ok {
				// Don't print stacktrace from goroutines twice
				lintCtx.Log.Warnf("Panic: %s: %s", pe, pe.Stack())
			} else {
				err = fmt.Errorf("panic occurred: %s", panicData)
				r.Log.Warnf("Panic stack trace: %s", debug.Stack())
			}
		}
	}()

	specificLintCtx := *lintCtx
	specificLintCtx.Log = r.Log.Child(lc.Name())

	issues, err := lc.Linter.Run(ctx, &specificLintCtx)
	if err != nil {
		return nil, err
	}

	for i := range issues {
		if issues[i].FromLinter == "" {
			issues[i].FromLinter = lc.Name()
		}
	}

	return issues, nil
}

type processorStat struct {
	inCount  int
	outCount int
}

func (r Runner) processLintResults(inIssues []result.Issue) []result.Issue {
	sw := timeutils.NewStopwatch("processing", r.Log)

	var issuesBefore, issuesAfter int
	statPerProcessor := map[string]processorStat{}

	var outIssues []result.Issue
	if len(inIssues) != 0 {
		issuesBefore += len(inIssues)
		outIssues = r.processIssues(inIssues, sw, statPerProcessor)
		issuesAfter += len(outIssues)
	}

	// finalize processors: logging, clearing, no heavy work here

	for _, p := range r.Processors {
		p := p
		sw.TrackStage(p.Name(), func() {
			p.Finish()
		})
	}

	if issuesBefore != issuesAfter {
		r.Log.Infof("Issues before processing: %d, after processing: %d", issuesBefore, issuesAfter)
	}
	r.printPerProcessorStat(statPerProcessor)
	sw.PrintStages()

	return outIssues
}

func (r Runner) printPerProcessorStat(stat map[string]processorStat) {
	parts := make([]string, 0, len(stat))
	for name, ps := range stat {
		if ps.inCount != 0 {
			parts = append(parts, fmt.Sprintf("%s: %d/%d", name, ps.outCount, ps.inCount))
		}
	}
	if len(parts) != 0 {
		r.Log.Infof("Processors filtering stat (out/in): %s", strings.Join(parts, ", "))
	}
}

func (r Runner) Run(ctx context.Context, linters []*linter.Config, lintCtx *linter.Context) ([]result.Issue, error) {
	sw := timeutils.NewStopwatch("linters", r.Log)
	defer sw.Print()

	var issues []result.Issue
	var runErr error
	for _, lc := range linters {
		lc := lc
		sw.TrackStage(lc.Name(), func() {
			linterIssues, err := r.runLinterSafe(ctx, lintCtx, lc)
			if err != nil {
				r.Log.Warnf("Can't run linter %s: %s", lc.Linter.Name(), err)
				if os.Getenv("GOLANGCI_COM_RUN") == "" {
					// Don't stop all linters on one linter failure for golangci.com.
					runErr = err
				}
				return
			}
			issues = append(issues, linterIssues...)
		})
	}

	return r.processLintResults(issues), runErr
}

func (r *Runner) processIssues(issues []result.Issue, sw *timeutils.Stopwatch, statPerProcessor map[string]processorStat) []result.Issue {
	for _, p := range r.Processors {
		var newIssues []result.Issue
		var err error
		p := p
		sw.TrackStage(p.Name(), func() {
			newIssues, err = p.Process(issues)
		})

		if err != nil {
			r.Log.Warnf("Can't process result by %s processor: %s", p.Name(), err)
		} else {
			stat := statPerProcessor[p.Name()]
			stat.inCount += len(issues)
			stat.outCount += len(newIssues)
			statPerProcessor[p.Name()] = stat
			issues = newIssues
		}

		if issues == nil {
			issues = []result.Issue{}
		}
	}

	return issues
}
