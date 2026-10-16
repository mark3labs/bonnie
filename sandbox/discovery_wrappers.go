package sandbox

// preserveSkillDiscovery keeps existing capabilities and execution routing.
func preserveSkillDiscovery(sb Sandbox, source Sandbox) Sandbox {
	discovery, ok := source.(SkillDiscovery)
	if !ok {
		return sb
	}
	reader, d := sb.(DirectoryReader)
	directory := directoryCapability{reader, directoryUsesExec(reader)}
	jobs, j := sb.(JobSandbox)
	output, o := sb.(JobOutputReader)
	deleter, x := sb.(Deleter)
	switch {
	case d && j && o && x:
		return &struct {
			Sandbox
			SkillDiscovery
			directoryCapability
			JobSandbox
			JobOutputReader
			Deleter
		}{sb, discovery, directory, jobs, output, deleter}
	case j && o && x:
		return &struct {
			Sandbox
			SkillDiscovery
			JobSandbox
			JobOutputReader
			Deleter
		}{sb, discovery, jobs, output, deleter}
	case d && o && x:
		return &struct {
			Sandbox
			SkillDiscovery
			directoryCapability
			JobOutputReader
			Deleter
		}{sb, discovery, directory, output, deleter}
	case d && j && x:
		return &struct {
			Sandbox
			SkillDiscovery
			directoryCapability
			JobSandbox
			Deleter
		}{sb, discovery, directory, jobs, deleter}
	case d && j && o:
		return &struct {
			Sandbox
			SkillDiscovery
			directoryCapability
			JobSandbox
			JobOutputReader
		}{sb, discovery, directory, jobs, output}
	case o && x:
		return &struct {
			Sandbox
			SkillDiscovery
			JobOutputReader
			Deleter
		}{sb, discovery, output, deleter}
	case j && x:
		return &struct {
			Sandbox
			SkillDiscovery
			JobSandbox
			Deleter
		}{sb, discovery, jobs, deleter}
	case j && o:
		return &struct {
			Sandbox
			SkillDiscovery
			JobSandbox
			JobOutputReader
		}{sb, discovery, jobs, output}
	case d && x:
		return &struct {
			Sandbox
			SkillDiscovery
			directoryCapability
			Deleter
		}{sb, discovery, directory, deleter}
	case d && o:
		return &struct {
			Sandbox
			SkillDiscovery
			directoryCapability
			JobOutputReader
		}{sb, discovery, directory, output}
	case d && j:
		return &struct {
			Sandbox
			SkillDiscovery
			directoryCapability
			JobSandbox
		}{sb, discovery, directory, jobs}
	case x:
		return &struct {
			Sandbox
			SkillDiscovery
			Deleter
		}{sb, discovery, deleter}
	case o:
		return &struct {
			Sandbox
			SkillDiscovery
			JobOutputReader
		}{sb, discovery, output}
	case j:
		return &struct {
			Sandbox
			SkillDiscovery
			JobSandbox
		}{sb, discovery, jobs}
	case d:
		return &struct {
			Sandbox
			SkillDiscovery
			directoryCapability
		}{sb, discovery, directory}
	case true:
		return &struct {
			Sandbox
			SkillDiscovery
		}{sb, discovery}
	}
	return sb
}
