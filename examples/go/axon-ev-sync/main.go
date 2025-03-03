package main

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/cortexapps/axon-go"
	pb "github.com/cortexapps/axon-go/.generated/proto/github.com/cortexapps/axon"
	"github.com/cortexapps/axon_apps/axon-ev-sync/data"
	"go.uber.org/zap"
)

func main() {

	// create our agent client and register a handler
	agentClient := axon.NewAxonAgent()

	_, err := agentClient.RegisterHandler(myEvDataIngester,
		axon.WithInvokeOption(pb.HandlerInvokeType_RUN_NOW, ""),
		axon.WithInvokeOption(pb.HandlerInvokeType_CRON_SCHEDULE, "30 2 * * *"),
	)

	if err != nil {
		log.Fatalf("Error registering handler: %v", err)
	}

	// Start the run process.  This will block and stream invocations
	ctx := context.Background()
	agentClient.Run(ctx)

}

// Here we have our example handler that will be called every one second
func myEvDataIngester(ctx axon.HandlerContext) interface{} {

	// token means more rows, see
	//
	// 1. Load the data
	loader := data.NewLoader(ctx.Logger())
	dataChannel, err := loader.Load()
	if err != nil {
		ctx.Logger().Error("ERROR loading data: %v\n", zap.Error(err))
		return nil
	}

	// we pull this data and set up the following structure
	//
	// Domain: Car Make
	//  Domain: Car Model
	//    vehicle:
	//			tag=VIN+Identifier
	//			groups=type,zip,city

	e := newEvCortexSync(ctx)

	err = e.ensureVehicleType()
	if err != nil {
		ctx.Logger().Error("Error ensuring vehicle type: %v", zap.Error(err))
		return nil
	}

	for ev := range dataChannel {
		makeDomainTag := e.ensureEntity("domain", ev.Make, ev.Make, nil, nil)
		name := fmt.Sprintf("%s %s", ev.Make, ev.Model)
		modelDomainTag := e.ensureEntity("domain", name, name, []string{
			ev.EvType(),
		},
			[]string{makeDomainTag},
		)

		e.ensureEntity(
			"vehicle",
			fmt.Sprintf("%d %s %s - %s...", ev.ModelYear, ev.Make, ev.Model, ev.VIN),
			fmt.Sprintf("%s-%d", ev.VIN, ev.DOLVehicleID),
			[]string{
				fmt.Sprintf("ev-type:%s", ev.EvType()),
				fmt.Sprintf("zip:%s", ev.PostalCode),
				fmt.Sprintf("city:%s", ev.City),
				fmt.Sprintf("model-year:%d", ev.ModelYear),
			},
			[]string{modelDomainTag},
		)
	}

	return nil

}

type evCortexSync struct {
	seenEntities   map[string]string
	entityTemplate *template.Template
	axonContext  axon.HandlerContext
	runKey         string
	logger         *zap.Logger
}

func newEvCortexSync(ctx axon.HandlerContext) *evCortexSync {
	return &evCortexSync{
		seenEntities:   make(map[string]string),
		axonContext:  ctx,
		entityTemplate: template.Must(template.New("entity").Parse(entityTemplate)),
		runKey:         time.Now().Format(time.RFC3339),
		logger:         ctx.Logger(),
	}
}

func (e *evCortexSync) ensureVehicleType() error {

	response, err := e.axonContext.CortexJsonApiCall(
		"POST",
		"/api/v1/catalog/definitions",
		`
		 {
            "description": "Type for vehicles (axon-ev-sync)",
            "name": "Vehicle",
            "schema": {},
            "type": "vehicle"
            }
		`)

	if err != nil {
		e.logger.Sugar().Errorf("Error creating entity type: %v", err)
		return err
	}

	if response.StatusCode == 409 {
		return nil
	}

	if response.StatusCode >= 400 {
		e.logger.Sugar().Errorf("Error creating entity type: %d", response.StatusCode)
		return fmt.Errorf("error creating entity type: %d", response.StatusCode)
	}
	return nil
}

func (e *evCortexSync) makeTag(name string) string {
	// convert any non-alphanumeric characters to underscores
	// make it lowercase
	// remove any leading or trailing underscores

	replaceRegEx := regexp.MustCompile("[^a-zA-Z0-9]+")
	tag := replaceRegEx.ReplaceAllString(name, "_")

	// replace any duplicate underscores with a single underscore
	tag = regexp.MustCompile("_+").ReplaceAllString(tag, "_")

	// remove any leading or trailing underscores
	return strings.Trim(strings.ToLower(tag), "_ ")
}

const entityTemplate = `
openapi: 3.0.1
info:
  title: {{.Name}}
  x-cortex-tag: {{.Tag}}-2
  x-cortex-type: {{.Type}}
{{ if .ParentDomainTags }}
  x-cortex-parents:{{range .ParentDomainTags}}
    - tag: {{.}}{{end}}
{{ end }}
{{ if .ServiceGroups }}
  x-cortex-service-groups:{{range .ServiceGroups}}
    - {{.}}{{end}}
{{ end }}
{{ if .CustomMetadata }}
  x-cortex-custom-metadata:{{range $key, $value := .CustomMetadata}}
    {{$key}}: {{$value}}{{end}}
{{ end }}
`

type entityTemplateData struct {
	Type             string
	Name             string
	Tag              string
	ParentDomainTags []string
	ServiceGroups    []string
	CustomMetadata   map[string]interface{}
}

func (e *evCortexSync) ensureEntity(entityType string, name string, tag string, serviceGroups []string, parentDomainTags []string) string {

	if tag == "" {
		tag = e.makeTag(name)
	}
	tag = e.makeTag(tag)

	if v, ok := e.seenEntities[tag]; ok {
		return v
	}

	entityYaml, err := e.makeEntity(entityTemplateData{
		Type:             entityType,
		Name:             name,
		Tag:              tag,
		ParentDomainTags: parentDomainTags,
		ServiceGroups:    serviceGroups,
		CustomMetadata:   map[string]interface{}{"last-run": e.runKey},
	})

	if err != nil {
		panic(err)
	}

	err = e.patchYaml(entityYaml)
	if err != nil {
		log.Println("Error patching yaml: ", err)
		log.Println("YAML was: ", entityYaml)
		return tag
	} else {
		e.logger.Sugar().Infof("Patched entity: %s (type=%s)\n", tag, entityType)
	}

	e.seenEntities[tag] = tag
	return tag

}

func (e *evCortexSync) patchYaml(yaml string) error {

	request := &pb.CallRequest{
		Method:      "PATCH",
		Path:        "/api/v1/open-api",
		Body:        yaml,
		ContentType: "application/openapi;charset=UTF-8",
	}
	resp, err := e.axonContext.Api().Call(
		context.Background(),
		request,
	)

	if err != nil {
		return err
	}

	if resp.StatusCode >= 400 {
		return fmt.Errorf("error patching yaml: %d", resp.StatusCode)
	}
	return nil
}

func (e *evCortexSync) makeEntity(data interface{}) (string, error) {
	var buf bytes.Buffer
	err := e.entityTemplate.Execute(&buf, data)
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}
