package main

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"

	Astounding "github.com/PbPipes/Astounding/pipeline"
	plugin "github.com/golang/protobuf/protoc-gen-go/plugin"
	"google.golang.org/protobuf/proto"

	"github.com/golang/glog"
	descriptor "github.com/golang/protobuf/protoc-gen-go/descriptor"
)

// pipeline.pb.go (in this directory) generated by the following -->
// protoc --go_out=. --go_opt=module=github.com/PbPipes/Astounding pipeline.proto

var (
	globalPkg = &ProtoPackage{
		name:     "",
		parent:   nil,
		children: make(map[string]*ProtoPackage),
		types:    make(map[string]*descriptor.DescriptorProto),
	}
)

// ProtoPackage describes a package of Protobuf, which is an container of message types.
type ProtoPackage struct {
	name     string
	parent   *ProtoPackage
	children map[string]*ProtoPackage
	types    map[string]*descriptor.DescriptorProto
}

func registerType(pkgName *string, msg *descriptor.DescriptorProto) {
	pkg := globalPkg
	if pkgName != nil {
		for _, node := range strings.Split(*pkgName, ".") {
			if pkg == globalPkg && node == "" {
				// Skips leading "."
				continue
			}

			child, ok := pkg.children[node]
			if !ok {
				child = &ProtoPackage{
					name:     pkg.name + "." + node,
					parent:   pkg,
					children: make(map[string]*ProtoPackage),
					types:    make(map[string]*descriptor.DescriptorProto),
				}
				pkg.children[node] = child
			}
			pkg = child
		}
	}

	pkg.types[msg.GetName()] = msg
}

func (pkg *ProtoPackage) lookupType(name string) (*descriptor.DescriptorProto, bool) {
	if strings.HasPrefix(name, ".") {
		return globalPkg.relativelyLookupType(name[1:len(name)])
	}

	for ; pkg != nil; pkg = pkg.parent {
		if desc, ok := pkg.relativelyLookupType(name); ok {
			return desc, ok
		}
	}
	return nil, false
}

func relativelyLookupNestedType(desc *descriptor.DescriptorProto, name string) (*descriptor.DescriptorProto, bool) {
	components := strings.Split(name, ".")
componentLoop:
	for _, component := range components {
		for _, nested := range desc.GetNestedType() {
			if nested.GetName() == component {
				desc = nested
				continue componentLoop
			}
		}
		glog.Infof("no such nested message %s in %s", component, desc.GetName())
		return nil, false
	}
	return desc, true
}

func (pkg *ProtoPackage) relativelyLookupType(name string) (*descriptor.DescriptorProto, bool) {
	components := strings.SplitN(name, ".", 2)
	switch len(components) {
	case 0:
		glog.V(1).Info("empty message name")
		return nil, false
	case 1:
		found, ok := pkg.types[components[0]]
		return found, ok
	case 2:
		glog.Infof("looking for %s in %s at %s (%v)", components[1], components[0], pkg.name, pkg)
		if child, ok := pkg.children[components[0]]; ok {
			found, ok := child.relativelyLookupType(components[1])
			return found, ok
		}
		if msg, ok := pkg.types[components[0]]; ok {
			found, ok := relativelyLookupNestedType(msg, components[1])
			return found, ok
		}
		glog.V(1).Infof("no such package nor message %s in %s", components[0], pkg.name)
		return nil, false
	default:
		glog.Fatal("not reached")
		return nil, false
	}
}

func (pkg *ProtoPackage) relativelyLookupPackage(name string) (*ProtoPackage, bool) {
	components := strings.Split(name, ".")
	for _, c := range components {
		var ok bool
		pkg, ok = pkg.children[c]
		if !ok {
			return nil, false
		}
	}
	return pkg, true
}

func getPipelineMessageOptions(msg *descriptor.DescriptorProto) (*Astounding.PipeOptions, error) {
	options := msg.GetOptions()
	if options == nil {
		return nil, nil
	}

	if !proto.HasExtension(options, Astounding.E_PipeOpts) {
		return nil, nil
	}

	return proto.GetExtension(options, Astounding.E_PipeOpts).(*Astounding.PipeOptions), nil
}

func convertFile(file *descriptor.FileDescriptorProto) ([]*plugin.CodeGeneratorResponse_File, error) {
	/*
		name := path.Base(file.GetName())
		pkg, ok := globalPkg.relativelyLookupPackage(file.GetPackage())
		if !ok {
			return nil, fmt.Errorf("no such package found: %s", file.GetPackage())
		}
	*/

	response := []*plugin.CodeGeneratorResponse_File{}
	for _, msg := range file.GetMessageType() {
		opts, err := getPipelineMessageOptions(msg)
		if err != nil {
			return nil, err
		}
		if opts == nil {
			continue
		}

		pubsubTopicName := opts.GetPubsubTopicName()

		resFile := &plugin.CodeGeneratorResponse_File{
			Name:    proto.String(fmt.Sprintf("%s/%s.schema", strings.Replace(file.GetPackage(), ".", "/", -1), pubsubTopicName)),
			Content: proto.String(string("pubSubTOPIC!")),
		}
		response = append(response, resFile)
	}

	return response, nil
}

func convert(req *plugin.CodeGeneratorRequest) (*plugin.CodeGeneratorResponse, error) {
	generateTargets := make(map[string]bool)
	for _, file := range req.GetFileToGenerate() {
		generateTargets[file] = true
	}

	res := &plugin.CodeGeneratorResponse{}
	for _, file := range req.GetProtoFile() {
		for _, msg := range file.GetMessageType() {
			glog.V(1).Infof("Loading a message type %s from package %s", msg.GetName(), file.GetPackage())
			registerType(file.Package, msg)
		}
	}
	for _, file := range req.GetProtoFile() {
		if _, ok := generateTargets[file.GetName()]; ok {
			glog.V(1).Info("Converting ", file.GetName())
			converted, err := convertFile(file)
			if err != nil {
				res.Error = proto.String(fmt.Sprintf("Failed to convert %s: %v", file.GetName(), err))
				return res, err
			}
			res.File = append(res.File, converted...)
		}
	}
	return res, nil
}

func convertFrom(rd io.Reader) (*plugin.CodeGeneratorResponse, error) {
	input, err := ioutil.ReadAll(rd)
	if err != nil {
		fmt.Errorf("Failed to read request:", err)
		return nil, err
	}
	req := &plugin.CodeGeneratorRequest{}
	err = proto.Unmarshal(input, req)
	if err != nil {
		glog.Error("Can't unmarshal input:", err)
		return nil, err
	}

	glog.V(1).Info("Converting input")
	return convert(req)
}

func main() {
	flag.Parse()
	ok := true
	glog.Info("Processing code generator request")
	res, err := convertFrom(os.Stdin)
	if err != nil {
		ok = false
		if res == nil {
			message := fmt.Sprintf("Failed to read input: %v", err)
			res = &plugin.CodeGeneratorResponse{
				Error: &message,
			}
		}
	}

	glog.Info("Serializing code generator response")
	data, err := proto.Marshal(res)
	if err != nil {
		glog.Fatal("Cannot marshal response", err)
	}
	_, err = os.Stdout.Write(data)
	if err != nil {
		glog.Fatal("Failed to write response", err)
	}

	if ok {
		glog.Info("Succeeded to process code generator request")
	} else {
		glog.Info("Failed to process code generator but successfully sent the error to protoc")
		os.Exit(1)
	}
}