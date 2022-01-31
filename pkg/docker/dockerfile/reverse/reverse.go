package reverse

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	docker "github.com/fsouza/go-dockerclient"
	"github.com/google/shlex"
	log "github.com/sirupsen/logrus"

	"github.com/docker-slim/docker-slim/pkg/consts"
	v "github.com/docker-slim/docker-slim/pkg/version"
)

// Dockerfile represents the reverse engineered Dockerfile info
type Dockerfile struct {
	Lines           []string
	Maintainers     []string
	AllUsers        []string
	ExeUser         string
	ExposedPorts    []string
	ImageStack      []*ImageInfo
	AllInstructions []*InstructionInfo
	HasOnbuild      bool
}

type ImageInfo struct {
	IsTopImage   bool               `json:"is_top_image"`
	ID           string             `json:"id"`
	FullName     string             `json:"full_name"`
	RepoName     string             `json:"repo_name"`
	VersionTag   string             `json:"version_tag"`
	RawTags      []string           `json:"raw_tags,omitempty"`
	CreateTime   string             `json:"create_time"`
	NewSize      int64              `json:"new_size"`
	NewSizeHuman string             `json:"new_size_human"`
	BaseImageID  string             `json:"base_image_id,omitempty"`
	Instructions []*InstructionInfo `json:"instructions"`
}

type InstructionInfo struct {
	Type string `json:"type"`
	Time string `json:"time"`
	//Time              time.Time `json:"time"`
	IsLastInstruction   bool     `json:"is_last_instruction,omitempty"`
	IsNop               bool     `json:"is_nop"`
	IsExecForm          bool     `json:"is_exec_form,omitempty"` //is exec/json format (a valid field for RUN, ENTRYPOINT, CMD)
	LocalImageExists    bool     `json:"local_image_exists"`
	IntermediateImageID string   `json:"intermediate_image_id,omitempty"`
	LayerIndex          int      `json:"layer_index"` //-1 for an empty layer
	LayerID             string   `json:"layer_id,omitempty"`
	LayerFSDiffID       string   `json:"layer_fsdiff_id,omitempty"`
	Size                int64    `json:"size"`
	SizeHuman           string   `json:"size_human,omitempty"`
	Params              string   `json:"params,omitempty"`
	CommandSnippet      string   `json:"command_snippet"`
	CommandAll          string   `json:"command_all"`
	SystemCommands      []string `json:"system_commands,omitempty"`
	Comment             string   `json:"comment,omitempty"`
	Author              string   `json:"author,omitempty"`
	EmptyLayer          bool     `json:"empty_layer,omitempty"`
	instPosition        string
	imageFullName       string
	RawTags             []string `json:"raw_tags,omitempty"`
	Target              string   `json:"target,omitempty"`      //for ADD and COPY
	SourceType          string   `json:"source_type,omitempty"` //for ADD and COPY
}

//The 'History' API doesn't expose the 'author' in the records it returns
//The 'author' field is useful in detecting if it's a Dockerfile instruction
//or if it's created with something else.
//One option is to combine the 'History' API data with the history data
//from the image config JSON embedded in the image.
//Another option is to rely on '#(nop)'.

const (
	defaultRunInstShell  = "/bin/sh"
	notRunInstPrefix     = "/bin/sh -c #(nop) "
	runInstShellPrefix   = "/bin/sh -c " //without any ARG params
	runInstArgsPrefix    = "|"
	instMaintainerPrefix = "MAINTAINER "
)

// DockerfileFromHistory recreates Dockerfile information from container image history
func DockerfileFromHistory(apiClient *docker.Client, imageID string) (*Dockerfile, error) {
	imageHistory, err := apiClient.ImageHistory(imageID)
	if err != nil {
		return nil, err
	}

	var out Dockerfile

	log.Debugf("\n\nIMAGE HISTORY =>\n%#v\n\n", imageHistory)

	var fatImageDockerInstructions []InstructionInfo
	var currentImageInfo *ImageInfo
	var prevImageID string

	imageLayerCount := len(imageHistory)
	imageLayerStart := imageLayerCount - 1
	startNewImage := true
	if imageLayerCount > 0 {
		for idx := imageLayerStart; idx >= 0; idx-- {
			isNop := false

			rawLine := imageHistory[idx].CreatedBy
			var inst string

			if strings.Contains(rawLine, "#(nop)") {
				isNop = true
			}

			isExecForm := false

			switch {
			case len(rawLine) == 0:
				inst = ""
			case strings.HasPrefix(rawLine, notRunInstPrefix):
				//Instructions that are not RUN
				inst = strings.TrimPrefix(rawLine, notRunInstPrefix)
			case strings.HasPrefix(rawLine, runInstShellPrefix):
				//RUN instruction in shell form
				runData := strings.TrimPrefix(rawLine, runInstShellPrefix)
				if strings.Contains(runData, "&&") {
					parts := strings.Split(runData, "&&")
					for i := range parts {
						partPrefix := ""
						if i != 0 {
							partPrefix = "\t"
						}
						parts[i] = partPrefix + strings.TrimSpace(parts[i])
					}
					runDataFormatted := strings.Join(parts, " && \\\n")
					inst = "RUN " + runDataFormatted
				} else {
					inst = "RUN " + runData
				}
			default:
				//TODO: need to refactor
				processed := false
				rawInst := rawLine
				if strings.HasPrefix(rawInst, runInstArgsPrefix) {
					parts := strings.SplitN(rawInst, " ", 2)
					if len(parts) == 2 {
						withArgs := strings.TrimSpace(parts[1])
						argNumStr := parts[0][1:]
						argNum, err := strconv.Atoi(argNumStr)
						if err == nil {
							if withArgsArray, err := shlex.Split(withArgs); err == nil {
								if len(withArgsArray) > argNum {
									rawInstParts := withArgsArray[argNum:]
									processed = true
									if len(rawInstParts) > 2 &&
										rawInstParts[0] == defaultRunInstShell &&
										rawInstParts[1] == "-c" {
										isExecForm = false
										rawInstParts = rawInstParts[2:]

										inst = fmt.Sprintf("RUN %s", strings.Join(rawInstParts, " "))
										inst = strings.TrimSpace(inst)
									} else {
										isExecForm = true

										var outJson bytes.Buffer
										encoder := json.NewEncoder(&outJson)
										encoder.SetEscapeHTML(false)
										err := encoder.Encode(rawInstParts)
										if err == nil {
											inst = fmt.Sprintf("RUN %s", outJson.String())
										}
									}
								} else {
									log.Infof("ReverseDockerfileFromHistory - RUN with ARGs - malformed - %v (%v)", rawInst, err)
								}
							} else {
								log.Infof("ReverseDockerfileFromHistory - RUN with ARGs - malformed - %v (%v)", rawInst, err)
							}

						} else {
							log.Infof("ReverseDockerfileFromHistory - RUN with ARGs - malformed number of ARGs - %v (%v)", rawInst, err)
						}
					} else {
						log.Infof("ReverseDockerfileFromHistory - RUN with ARGs - unexpected number of parts - %v", rawInst)
					}
				}

				if !processed {
					//default to RUN instruction in exec form
					isExecForm = true
					inst = "RUN " + rawInst
					if outArray, err := shlex.Split(rawInst); err == nil {
						var outJson bytes.Buffer
						encoder := json.NewEncoder(&outJson)
						encoder.SetEscapeHTML(false)
						err := encoder.Encode(outArray)
						if err == nil {
							inst = fmt.Sprintf("RUN %s", outJson.String())
						}
					}
				}
			}

			//NOTE: Dockerfile instructions can be any case, but the instructions from history are always uppercase
			cleanInst := strings.TrimSpace(inst)

			if strings.HasPrefix(cleanInst, "ENTRYPOINT ") {
				cleanInst = strings.Replace(cleanInst, "&{[", "[", -1)
				cleanInst = strings.Replace(cleanInst, "]}", "]", -1)

				entrypointShellFormPrefix := `ENTRYPOINT ["/bin/sh" "-c" "`
				if strings.HasPrefix(cleanInst, entrypointShellFormPrefix) {
					instData := strings.TrimPrefix(cleanInst, entrypointShellFormPrefix)
					instData = strings.TrimSuffix(instData, `"]`)
					cleanInst = "ENTRYPOINT " + instData
				} else {
					isExecForm = true

					instData := strings.TrimPrefix(cleanInst, "ENTRYPOINT ")
					instData = fixJSONArray(instData)
					cleanInst = "ENTRYPOINT " + instData
				}
			}

			if strings.HasPrefix(cleanInst, "CMD ") {
				cmdShellFormPrefix := `CMD ["/bin/sh" "-c" "`
				if strings.HasPrefix(cleanInst, cmdShellFormPrefix) {
					instData := strings.TrimPrefix(cleanInst, cmdShellFormPrefix)
					instData = strings.TrimSuffix(instData, `"]`)
					cleanInst = "CMD " + instData
				} else {
					isExecForm = true

					instData := strings.TrimPrefix(cleanInst, "CMD ")
					instData = fixJSONArray(instData)
					cleanInst = "CMD " + instData
				}
			}

			if strings.HasPrefix(cleanInst, instMaintainerPrefix) {
				parts := strings.SplitN(cleanInst, " ", 2)
				if len(parts) == 2 {
					maintainer := strings.TrimSpace(parts[1])

					out.Maintainers = append(out.Maintainers, maintainer)
				} else {
					log.Infof("ReverseDockerfileFromHistory - MAINTAINER - unexpected number of user parts - %v", len(parts))
				}
			}

			if strings.HasPrefix(cleanInst, "USER ") {
				parts := strings.SplitN(cleanInst, " ", 2)
				if len(parts) == 2 {
					userName := strings.TrimSpace(parts[1])

					out.AllUsers = append(out.AllUsers, userName)
					out.ExeUser = userName
				} else {
					log.Infof("ReverseDockerfileFromHistory - unexpected number of user parts - %v", len(parts))
				}
			}

			if strings.HasPrefix(cleanInst, "EXPOSE ") {
				parts := strings.SplitN(cleanInst, " ", 2)
				if len(parts) == 2 {
					portInfo := strings.TrimSpace(parts[1])

					out.ExposedPorts = append(out.ExposedPorts, portInfo)
				} else {
					log.Infof("ReverseDockerfileFromHistory - unexpected number of expose parts - %v", len(parts))
				}
			}

			instInfo := InstructionInfo{
				IsNop:      isNop,
				IsExecForm: isExecForm,
				CommandAll: cleanInst,
				Time:       time.Unix(imageHistory[idx].Created, 0).UTC().Format(time.RFC3339),
				//Time:    time.Unix(imageHistory[idx].Created, 0),
				Comment: imageHistory[idx].Comment,
				RawTags: imageHistory[idx].Tags,
				Size:    imageHistory[idx].Size,
			}

			instParts := strings.SplitN(cleanInst, " ", 2)
			if len(instParts) == 2 {
				instInfo.Type = instParts[0]
			}

			if instInfo.Type == "ONBUILD" {
				out.HasOnbuild = true
			}

			if instInfo.CommandAll == "" {
				instInfo.Type = "NONE"
				instInfo.CommandAll = "#no instruction info"
			}

			if instInfo.Type == "RUN" {
				var cmdParts []string
				cmds := strings.Replace(instParts[1], "\\", "", -1)
				if strings.Contains(cmds, "&&") {
					cmdParts = strings.Split(cmds, "&&")
				} else {
					cmdParts = strings.Split(cmds, ";")
				}

				for _, cmd := range cmdParts {
					cmd = strings.TrimSpace(cmd)
					cmd = strings.Replace(cmd, "\t", "", -1)
					cmd = strings.Replace(cmd, "\n", "", -1)
					instInfo.SystemCommands = append(instInfo.SystemCommands, cmd)
				}
			} else {
				if len(instParts) == 2 {
					instInfo.Params = instParts[1]
				}
			}

			if instInfo.Type == "WORKDIR" {
				instInfo.SystemCommands = append(instInfo.SystemCommands, fmt.Sprintf("mkdir -p %s", instParts[1]))
			}

			switch instInfo.Type {
			case "ADD", "COPY":
				pparts := strings.SplitN(instInfo.Params, ":", 2)
				if len(pparts) == 2 {
					instInfo.SourceType = pparts[0]
					tparts := strings.SplitN(pparts[1], " in ", 2)
					if len(tparts) == 2 {
						instInfo.Target = tparts[1]

						instInfo.CommandAll = fmt.Sprintf("%s %s:%s %s",
							instInfo.Type,
							instInfo.SourceType,
							tparts[0],
							instInfo.Target)
					}
				}
			}

			if instInfo.Type == "HEALTHCHECK" {
				//TODO: restore the HEALTHCHECK instruction
				//Example:
				// HEALTHCHECK &{["CMD" "/healthcheck" "8080"] "5s" "10s" "0s" '\x03'}
				// HEALTHCHECK --interval=5s --timeout=10s --retries=3 CMD [ "/healthcheck", "8080" ]
			}

			if len(instInfo.CommandAll) > 44 {
				instInfo.CommandSnippet = fmt.Sprintf("%s...", instInfo.CommandAll[0:44])
			} else {
				instInfo.CommandSnippet = instInfo.CommandAll
			}

			if instInfo.Size > 0 {
				instInfo.SizeHuman = humanize.Bytes(uint64(instInfo.Size))
			}

			if imageHistory[idx].ID != "<missing>" {
				instInfo.LocalImageExists = true
				instInfo.IntermediateImageID = imageHistory[idx].ID
			}

			if startNewImage {
				startNewImage = false
				currentImageInfo = &ImageInfo{
					BaseImageID: prevImageID,
					NewSize:     0,
				}
			}

			currentImageInfo.NewSize += imageHistory[idx].Size
			currentImageInfo.Instructions = append(currentImageInfo.Instructions, &instInfo)

			out.AllInstructions = append(out.AllInstructions, &instInfo)

			instPosition := "intermediate"
			if idx == imageLayerStart {
				instPosition = "first" //first instruction in the list
			}

			if idx == 0 || (len(imageHistory[idx].Tags) > 0) {
				instPosition = "last" //last in an image

				currentImageInfo.ID = imageHistory[idx].ID
				prevImageID = currentImageInfo.ID

				if instInfo.IntermediateImageID == currentImageInfo.ID {
					instInfo.IntermediateImageID = ""
					instInfo.IsLastInstruction = true
				}

				currentImageInfo.CreateTime = instInfo.Time
				currentImageInfo.RawTags = imageHistory[idx].Tags

				if len(imageHistory[idx].Tags) > 0 {
					instInfo.imageFullName = imageHistory[idx].Tags[0]
					currentImageInfo.FullName = imageHistory[idx].Tags[0]

					if tagInfo := strings.Split(imageHistory[idx].Tags[0], ":"); len(tagInfo) > 1 {
						currentImageInfo.RepoName = tagInfo[0]
						currentImageInfo.VersionTag = tagInfo[1]
					}
				}

				currentImageInfo.NewSizeHuman = humanize.Bytes(uint64(currentImageInfo.NewSize))

				out.ImageStack = append(out.ImageStack, currentImageInfo)
				startNewImage = true
			}

			instInfo.instPosition = instPosition

			fatImageDockerInstructions = append(fatImageDockerInstructions, instInfo)
		}

		if currentImageInfo != nil {
			currentImageInfo.IsTopImage = true
		}
	}

	//Always adding "FROM scratch" as the first line
	//GOAL: to have a reversed Dockerfile that can be used to build a new image
	out.Lines = append(out.Lines, "FROM scratch")
	for idx, instInfo := range fatImageDockerInstructions {
		if instInfo.instPosition == "first" {
			out.Lines = append(out.Lines, "# new image")
		}

		out.Lines = append(out.Lines, instInfo.CommandAll)
		if instInfo.instPosition == "last" {
			commentText := fmt.Sprintf("# end of image: %s (id: %s tags: %s)",
				instInfo.imageFullName, instInfo.IntermediateImageID, strings.Join(instInfo.RawTags, ","))

			out.Lines = append(out.Lines, commentText)
			out.Lines = append(out.Lines, "")
			if idx < (len(fatImageDockerInstructions) - 1) {
				out.Lines = append(out.Lines, "# new image")
			}
		}

		if instInfo.Comment != "" {
			out.Lines = append(out.Lines, "# "+instInfo.Comment)
		}

		//TODO: use time diff to separate each instruction
	}

	log.Debugf("IMAGE INSTRUCTIONS:")
	for _, iiLine := range out.Lines {
		log.Debug(iiLine)
	}

	return &out, nil

	/*
	   TODO:
	   need to have a set of signature for common base images
	   long path: need to discover base images dynamically
	   https://imagelayers.io/?images=alpine:3.1,ubuntu:14.04.1&lock=alpine:3.1

	   https://imagelayers.io/
	   https://github.com/CenturyLinkLabs/imagelayers
	   https://github.com/CenturyLinkLabs/imagelayers-graph
	*/
}

// SaveDockerfileData saves the Dockerfile information to a file
func SaveDockerfileData(fatImageDockerfileLocation string, fatImageDockerfileLines []string) error {
	var data bytes.Buffer
	data.WriteString(strings.Join(fatImageDockerfileLines, "\n"))
	return ioutil.WriteFile(fatImageDockerfileLocation, data.Bytes(), 0644)
}

// GenerateFromInfo builds and saves a Dockerfile file object
func GenerateFromInfo(location string,
	volumes map[string]struct{},
	workingDir string,
	env []string,
	labels map[string]string,
	user string,
	exposedPorts map[docker.Port]struct{},
	entrypoint []string,
	cmd []string,
	hasData bool,
	tarData bool) error {

	dockerfileLocation := filepath.Join(location, "Dockerfile")

	var dfData bytes.Buffer
	dfData.WriteString("FROM scratch\n")

	dsInfoLabel := fmt.Sprintf("LABEL %s=\"%s\"\n", consts.ContainerLabelName, v.Current())
	dfData.WriteString(dsInfoLabel)

	if len(labels) > 0 {
		for name, value := range labels {
			var encoded bytes.Buffer
			encoder := json.NewEncoder(&encoded)
			encoder.SetEscapeHTML(false)
			encoder.Encode(value)
			labelInfo := fmt.Sprintf("LABEL %s=%s\n", name, encoded.String())
			dfData.WriteString(labelInfo)
		}
		dfData.WriteByte('\n')
	}

	if len(env) > 0 {
		for _, envInfo := range env {
			if envParts := strings.Split(envInfo, "="); len(envParts) > 1 {
				dfData.WriteString("ENV ")
				envLine := fmt.Sprintf("%s \"%s\"", envParts[0], envParts[1])
				dfData.WriteString(envLine)
				dfData.WriteByte('\n')
			}
		}
		dfData.WriteByte('\n')
	}

	if len(volumes) > 0 {
		var volumeList []string
		for volumeName := range volumes {
			volumeList = append(volumeList, strconv.Quote(volumeName))
		}

		volumeInst := fmt.Sprintf("VOLUME [%s]", strings.Join(volumeList, ","))
		dfData.WriteString(volumeInst)
		dfData.WriteByte('\n')
	}

	if hasData {
		addData := "COPY files /\n"
		if tarData {
			addData = "ADD files.tar /\n"
		}

		dfData.WriteString(addData)
	}

	if workingDir != "" {
		dfData.WriteString("WORKDIR ")
		dfData.WriteString(workingDir)
		dfData.WriteByte('\n')
	}

	if user != "" {
		dfData.WriteString("USER ")
		dfData.WriteString(user)
		dfData.WriteByte('\n')
	}

	if len(exposedPorts) > 0 {
		for portInfo := range exposedPorts {
			dfData.WriteString("EXPOSE ")
			dfData.WriteString(string(portInfo))
			dfData.WriteByte('\n')
		}
	}

	if len(entrypoint) > 0 {
		//TODO: need to make sure the generated ENTRYPOINT is compatible with the original behavior
		var quotedEntryPoint []string
		for idx := range entrypoint {
			quotedEntryPoint = append(quotedEntryPoint, strconv.Quote(entrypoint[idx]))
		}

		dfData.WriteString("ENTRYPOINT [")
		dfData.WriteString(strings.Join(quotedEntryPoint, ","))
		dfData.WriteByte(']')
		dfData.WriteByte('\n')
	}

	if len(cmd) > 0 {
		//TODO: need to make sure the generated CMD is compatible with the original behavior
		var quotedCmd []string
		for idx := range cmd {
			quotedCmd = append(quotedCmd, strconv.Quote(cmd[idx]))
		}
		dfData.WriteString("CMD [")
		dfData.WriteString(strings.Join(quotedCmd, ","))
		dfData.WriteByte(']')
		dfData.WriteByte('\n')
	}

	return ioutil.WriteFile(dockerfileLocation, dfData.Bytes(), 0644)
}

func fixJSONArray(in string) string {
	data := in
	if data[0] == '[' {
		data = data[1 : len(data)-1]
	}
	outArray, err := shlex.Split(data)
	if err != nil {
		return in
	}

	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	err = encoder.Encode(outArray)
	if err != nil {
		return in
	}

	return out.String()
}

func deserialiseHealtheckInstruction(config *docker.HealthConfig) (string, *docker.HealthConfig, error) {

	data := `HEALTHCHECK &{["CMD" "/healthcheck" "8080"] "5s" "10s" "2s" '\x03'}`
	cleanInst := strings.TrimSpace(data)
	var (
		instPart1 string
		instPart2 string
		instParts []string
	)
	if strings.HasPrefix(cleanInst, "HEALTHCHECK ") {

		cleanInst = strings.Replace(cleanInst, "&{[", "", -1)

		//Splits the string into two parts - first part pointer to array of string and rest of the string with } in end.
		instParts = strings.SplitN(cleanInst, "]", 2)

		// Cleans HEALTHCHECK part and splits the first part further
		parts := strings.SplitN(instParts[0], " ", 2)

		// joins the first part of the string
		instPart1 = strings.Join(parts[1:], " ")

		// removes quotes from the first part of the string
		instPart1 = strings.ReplaceAll(instPart1, "\"", "")

		// cleans it to assign it to the pointer config.Test
		config.Test = strings.Split(instPart1, " ")

		// removes the } from the second part of the string
		instPart2 = strings.Replace(instParts[1], "}", "", -1)

		// removes extra spaces from string
		instPart2 = strings.TrimSpace(instPart2)

	}

	_, err := fmt.Sscanf(instPart2, `"%ds" "%ds" "%ds" '\x%x'`, &config.Interval, &config.Timeout, &config.StartPeriod, &config.Retries)

	if err != nil {
		panic(err)
	}

	healthInst := fmt.Sprintf(`HEALTHCHECK --interval=%ds --timeout=%ds --start-period=%ds --retries=%x %s`, config.Interval, config.Timeout, config.StartPeriod, config.Retries, strings.Join(config.Test, " "))
	fmt.Println(healthInst)

	// returns: healthinst and &{[CMD /healthcheck 8080] 5ns 10ns 2ns 3}
	return healthInst, config, nil
}

//
// https://docs.docker.com/engine/reference/builder/
//
