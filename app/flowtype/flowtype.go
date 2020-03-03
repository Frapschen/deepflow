package flowtype

//go:generate tmpl -data=@codes.tmpldata -o codes.go ../common/gen/codes.go.tmpl

import (
	"net"

	"gitlab.x.lan/yunshan/droplet-libs/app"
	"gitlab.x.lan/yunshan/droplet-libs/codec"
	inputtype "gitlab.x.lan/yunshan/droplet-libs/datatype"
	"gitlab.x.lan/yunshan/droplet-libs/utils"
	outputtype "gitlab.x.lan/yunshan/droplet-libs/zerodoc"
	. "gitlab.x.lan/yunshan/droplet/app/common/docbuffer"
	. "gitlab.x.lan/yunshan/droplet/app/common/doctime"
	. "gitlab.x.lan/yunshan/droplet/app/common/endpoint"
	. "gitlab.x.lan/yunshan/droplet/app/common/flow"
	. "gitlab.x.lan/yunshan/droplet/app/common/policy"

	"github.com/google/gopacket/layers"
	logging "github.com/op/go-logging"
)

// 注意：仅统计TCP流

var log = logging.MustGetLogger("flowtype")

const (
	CODES_LEN  = 64
	GROUPS_LEN = 16
)

type FlowToTypeDocumentMapper struct {
	policyGroup []inputtype.AclAction

	docs      *utils.StructBuffer
	encoder   *codec.SimpleEncoder
	codes     []outputtype.Code
	aclGroups [2][]int32

	fields [2]outputtype.Field
	meters [inputtype.MaxCloseType]outputtype.TypeMeter

	isInterestCloseType [inputtype.MaxCloseType]bool
}

func (p *FlowToTypeDocumentMapper) GetName() string {
	return "FlowToTypeDocumentMapper"
}

func NewProcessor() app.FlowProcessor {
	return &FlowToTypeDocumentMapper{}
}

func (p *FlowToTypeDocumentMapper) Prepare() {
	p.docs = NewMeterSharedDocBuffer()
	p.policyGroup = make([]inputtype.AclAction, 0)
	p.encoder = &codec.SimpleEncoder{}
	p.codes = make([]outputtype.Code, 0, CODES_LEN)
	p.aclGroups = [2][]int32{make([]int32, 0, GROUPS_LEN), make([]int32, 0, GROUPS_LEN)}

	for flowType := inputtype.CloseType(0); flowType < inputtype.MaxCloseType; flowType++ {
		switch flowType {
		case inputtype.CloseTypeTCPServerRst:
			p.meters[flowType].SumCountTServerRst = 1
			p.isInterestCloseType[flowType] = true
		case inputtype.CloseTypeTCPClientRst:
			p.meters[flowType].SumCountTClientRst = 1
			p.isInterestCloseType[flowType] = true
		case inputtype.CloseTypeServerHalfOpen:
			p.meters[flowType].SumCountTServerHalfOpen = 1
			p.isInterestCloseType[flowType] = true
		case inputtype.CloseTypeClientHalfOpen:
			p.meters[flowType].SumCountTClientHalfOpen = 1
			p.isInterestCloseType[flowType] = true
		case inputtype.CloseTypeServerHalfClose:
			p.meters[flowType].SumCountTServerHalfClose = 1
			p.isInterestCloseType[flowType] = true
		case inputtype.CloseTypeClientHalfClose:
			p.meters[flowType].SumCountTClientHalfClose = 1
			p.isInterestCloseType[flowType] = true
		}
	}
}

func (p *FlowToTypeDocumentMapper) Process(rawFlow *inputtype.TaggedFlow, variedTag bool) []interface{} {
	p.docs.Reset()

	if rawFlow.Proto != layers.IPProtocolTCP {
		return p.docs.Slice()
	}
	interestActionFlags := inputtype.ACTION_FLOW_MISC_COUNTING
	if rawFlow.PolicyData.ActionFlags&interestActionFlags == 0 {
		return p.docs.Slice()
	}
	flow := Flow(*rawFlow)
	if !flow.IsClosedFlow() {
		return p.docs.Slice()
	}
	if flow.CloseType >= inputtype.MaxCloseType || !p.isInterestCloseType[flow.CloseType] {
		return p.docs.Slice()
	}

	statTemplates := GetTagTemplateByActionFlags(&rawFlow.PolicyData, interestActionFlags)
	p.policyGroup = FillPolicyTagTemplate(&rawFlow.PolicyData, interestActionFlags, p.policyGroup)

	flowMetricsPeerSrc := &flow.FlowMetricsPeers[inputtype.FLOW_METRICS_PEER_SRC]
	flowMetricsPeerDst := &flow.FlowMetricsPeers[inputtype.FLOW_METRICS_PEER_DST]

	l3EpcIDs := [2]int32{flowMetricsPeerSrc.L3EpcID, flowMetricsPeerDst.L3EpcID}
	ips := [2]uint32{flow.IPSrc, flow.IPDst}
	ip6s := [2]net.IP{flow.IP6Src, flow.IP6Dst}
	// 虚拟网络流量用is_l2_end和is_l3_end去重
	// 接入网络流量只有一份，不去重
	statsEndpoint := [2]bool{true, true}
	if TOR.IsPortInRange(flow.InPort) {
		statsEndpoint[0] = flowMetricsPeerSrc.IsL2End && flowMetricsPeerSrc.IsL3End
		statsEndpoint[1] = flowMetricsPeerDst.IsL2End && flowMetricsPeerDst.IsL3End
	}
	directions := [2]outputtype.DirectionEnum{outputtype.ClientToServer, outputtype.ServerToClient}
	docTimestamp := RoundToMinute(flow.StartTime)

	isActiveHost := [2]bool{flowMetricsPeerSrc.IsActiveHost, flowMetricsPeerDst.IsActiveHost}
	for i := range ips {
		if !isActiveHost[i] || IsOuterPublicIp(l3EpcIDs[i]) {
			ips[i] = 0
			ip6s[i] = net.IPv6zero
		}
	}

	for _, thisEnd := range [...]EndPoint{ZERO, ONE} {
		if !statsEndpoint[thisEnd] {
			continue
		}
		otherEnd := GetOppositeEndpoint(thisEnd)

		meter := &p.meters[flow.CloseType]

		field := &p.fields[thisEnd]
		if flow.EthType == layers.EthernetTypeIPv4 {
			field.IsIPv6 = 0
			field.IP = ips[thisEnd]
			field.IP1 = ips[otherEnd]
		} else {
			field.IsIPv6 = 1
			field.IP6 = ip6s[thisEnd]
			field.IP61 = ip6s[otherEnd]
		}
		field.TAPType = TAPTypeFromInPort(flow.InPort)
		field.Direction = directions[thisEnd]
		field.Protocol = flow.Proto
		field.ServerPort = flow.PortDst

		// node
		if statTemplates&inputtype.TEMPLATE_NODE != 0 {
			for _, code := range NODE_CODES {
				if thisEnd == ONE && code.IsSymmetric() {
					continue
				}
				doc := p.docs.Get().(*app.Document)
				doc.Timestamp = docTimestamp
				field.FillTag(code, doc.Tag.(*outputtype.Tag))
				doc.Meter = meter
			}
		}

		// policy
		for _, policy := range p.policyGroup {
			codes := p.codes[:0]
			if policy.GetTagTemplates()&inputtype.TEMPLATE_ACL_NODE != 0 {
				codes = append(codes, POLICY_NODE_CODES...)
			}
			if policy.GetTagTemplates()&inputtype.TEMPLATE_ACL_NODE_PORT != 0 && flow.IsActiveService { // 含有端口号的，仅统计活跃端口
				codes = append(codes, POLICY_NODE_PORT_CODES...)
			}
			field.ACLGID = uint16(policy.GetACLGID())
			for _, code := range codes {
				if thisEnd == ONE && code.IsSymmetric() {
					continue
				}
				doc := p.docs.Get().(*app.Document)
				doc.Timestamp = docTimestamp
				field.FillTag(code, doc.Tag.(*outputtype.Tag))
				doc.Meter = meter
			}

			codes = p.codes[:0]
			if policy.GetTagTemplates()&inputtype.TEMPLATE_ACL_EDGE != 0 {
				codes = append(codes, POLICY_EDGE_CODES...)
			}
			if policy.GetTagTemplates()&inputtype.TEMPLATE_ACL_EDGE_PORT != 0 && flow.IsActiveService { // 含有端口号的，仅统计活跃端口
				codes = append(codes, POLICY_EDGE_PORT_CODES...)
			}
			for _, code := range codes {
				doc := p.docs.Get().(*app.Document)
				doc.Timestamp = docTimestamp
				field.FillTag(code, doc.Tag.(*outputtype.Tag))
				doc.Meter = meter
			}
		}
	}
	return p.docs.Slice()
}
