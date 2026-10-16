package sandbox

import (
	"context"

	"github.com/mark3labs/bonnie/internal/discoveryio"
)

// DiscoveryEntry describes a workspace child without following links.
type DiscoveryEntry = discoveryio.Entry

// DiscoveryFile contains bounded bytes and before/after file identities.
type DiscoveryFile = discoveryio.File

// DiscoveryReader is the optional secure workspace discovery capability.
// Paths are exact slash-separated workspace-relative paths. Implementations
// must reject symlinks in every component and bound bytes while reading.
type DiscoveryReader interface {
	ListDiscovery(context.Context, string, int) ([]DiscoveryEntry, bool, error)
	ReadDiscovery(context.Context, string, int64) (DiscoveryFile, error)
}

func preserveDiscovery(sb Sandbox, inner Sandbox) Sandbox {
	reader, ok := inner.(DiscoveryReader)
	if !ok {
		return sb
	}
	usesExec := directoryUsesExec(sb)

	jobs, j := sb.(JobSandbox)
	output, o := sb.(JobOutputReader)
	deleter, d := sb.(Deleter)
	directory, r := sb.(DirectoryReader)
	switch {
	case !j && !o && !d && !r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
		}{sb, discoveryExecMarker{usesExec}, reader}
	case j && !o && !d && !r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
			JobSandbox
		}{sb, discoveryExecMarker{usesExec}, reader, jobs}
	case !j && o && !d && !r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
			JobOutputReader
		}{sb, discoveryExecMarker{usesExec}, reader, output}
	case j && o && !d && !r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
			JobSandbox
			JobOutputReader
		}{sb, discoveryExecMarker{usesExec}, reader, jobs, output}
	case !j && !o && d && !r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
			Deleter
		}{sb, discoveryExecMarker{usesExec}, reader, deleter}
	case j && !o && d && !r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
			JobSandbox
			Deleter
		}{sb, discoveryExecMarker{usesExec}, reader, jobs, deleter}
	case !j && o && d && !r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
			JobOutputReader
			Deleter
		}{sb, discoveryExecMarker{usesExec}, reader, output, deleter}
	case j && o && d && !r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
			JobSandbox
			JobOutputReader
			Deleter
		}{sb, discoveryExecMarker{usesExec}, reader, jobs, output, deleter}
	case !j && !o && !d && r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
			DirectoryReader
		}{sb, discoveryExecMarker{usesExec}, reader, directory}
	case j && !o && !d && r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
			JobSandbox
			DirectoryReader
		}{sb, discoveryExecMarker{usesExec}, reader, jobs, directory}
	case !j && o && !d && r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
			JobOutputReader
			DirectoryReader
		}{sb, discoveryExecMarker{usesExec}, reader, output, directory}
	case j && o && !d && r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
			JobSandbox
			JobOutputReader
			DirectoryReader
		}{sb, discoveryExecMarker{usesExec}, reader, jobs, output, directory}
	case !j && !o && d && r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
			Deleter
			DirectoryReader
		}{sb, discoveryExecMarker{usesExec}, reader, deleter, directory}
	case j && !o && d && r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
			JobSandbox
			Deleter
			DirectoryReader
		}{sb, discoveryExecMarker{usesExec}, reader, jobs, deleter, directory}
	case !j && o && d && r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
			JobOutputReader
			Deleter
			DirectoryReader
		}{sb, discoveryExecMarker{usesExec}, reader, output, deleter, directory}
	case j && o && d && r:
		return &struct {
			Sandbox
			discoveryExecMarker
			DiscoveryReader
			JobSandbox
			JobOutputReader
			Deleter
			DirectoryReader
		}{sb, discoveryExecMarker{usesExec}, reader, jobs, output, deleter, directory}
	}
	return sb
}

type discoveryExecMarker struct{ usesExec bool }

func (m discoveryExecMarker) directoryExec() bool { return m.usesExec }
