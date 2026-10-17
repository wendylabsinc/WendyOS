package commands

import (
	"context"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

type legacyUnenrollServer struct {
	cloudpb.UnimplementedCertificateServiceServer
	cloudpb.UnimplementedAssetServiceServer
	cloudpb.UnimplementedOrganizationServiceServer
	mu            sync.Mutex
	events        []string
	revokeError   bool
	revokeSuccess bool
	deleteSuccess bool
	deleteErr     error
}

func (s *legacyUnenrollServer) ListCertificates(req *cloudpb.ListCertificatesRequest, stream grpc.ServerStreamingServer[cloudpb.ListCertificatesResponse]) error {
	if req.GetAssetId() != 42 {
		return status.Error(codes.InvalidArgument, "wrong asset")
	}
	for _, cert := range []*cloudpb.Certificate{
		{Id: 11, Status: cloudpb.CertificateStatus_CERTIFICATE_STATUS_ACTIVE},
		{Id: 12, Status: cloudpb.CertificateStatus_CERTIFICATE_STATUS_REVOKED},
		nil,
		{Id: 13, Status: cloudpb.CertificateStatus_CERTIFICATE_STATUS_ACTIVE},
		{Id: 14, Status: cloudpb.CertificateStatus_CERTIFICATE_STATUS_EXPIRED},
	} {
		if err := stream.Send(&cloudpb.ListCertificatesResponse{Certificate: cert}); err != nil {
			return err
		}
	}
	return nil
}

func (s *legacyUnenrollServer) RevokeCertificate(_ context.Context, req *cloudpb.RevokeCertificateRequest) (*cloudpb.RevokeCertificateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, fmt.Sprintf("revoke %d", req.GetCertificateId()))
	if s.revokeError && req.GetCertificateId() == 13 {
		return nil, status.Error(codes.PermissionDenied, "revocation refused")
	}
	return &cloudpb.RevokeCertificateResponse{Success: s.revokeSuccess}, nil
}

func (s *legacyUnenrollServer) DeleteAsset(_ context.Context, req *cloudpb.DeleteAssetRequest) (*cloudpb.DeleteAssetResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, fmt.Sprintf("delete %d", req.GetId()))
	if s.deleteErr != nil {
		return nil, s.deleteErr
	}
	return &cloudpb.DeleteAssetResponse{Success: s.deleteSuccess}, nil
}

func TestLegacyCloudUnenrollRequiresRevocationBeforeDeletion(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		revokeError, revokeSuccess bool
		deleteSuccess              bool
		wantRevoked                int
		wantDeleted                bool
		wantError                  string
		wantEvents                 []string
	}{
		{name: "success", revokeSuccess: true, deleteSuccess: true, wantRevoked: 2, wantDeleted: true, wantEvents: []string{"revoke 11", "revoke 13", "delete 42"}},
		{name: "revocation error", revokeError: true, revokeSuccess: true, deleteSuccess: true, wantRevoked: 1, wantError: "revoking certificate 13", wantEvents: []string{"revoke 11", "revoke 13"}},
		{name: "revocation not confirmed", deleteSuccess: true, wantError: "confirm revocation", wantEvents: []string{"revoke 11"}},
		{name: "deletion not confirmed", revokeSuccess: true, wantRevoked: 2, wantError: "confirm deletion", wantEvents: []string{"revoke 11", "revoke 13", "delete 42"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &legacyUnenrollServer{revokeError: tc.revokeError, revokeSuccess: tc.revokeSuccess, deleteSuccess: tc.deleteSuccess}
			listener := bufconn.Listen(1 << 20)
			server := grpc.NewServer()
			cloudpb.RegisterCertificateServiceServer(server, service)
			cloudpb.RegisterAssetServiceServer(server, service)
			go server.Serve(listener)
			t.Cleanup(server.Stop)
			t.Cleanup(func() { listener.Close() })
			conn, err := grpc.NewClient("passthrough:///legacy-unenroll", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close() })
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			revoked, deleted, err := cloudUnenrollCleanupClient(ctx, conn, 42)
			if revoked != tc.wantRevoked || deleted != tc.wantDeleted || (err == nil) != (tc.wantError == "") || err != nil && !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("got revoked=%d deleted=%v err=%v, want %d %v %q", revoked, deleted, err, tc.wantRevoked, tc.wantDeleted, tc.wantError)
			}
			service.mu.Lock()
			defer service.mu.Unlock()
			if !reflect.DeepEqual(service.events, tc.wantEvents) {
				t.Fatalf("wrong operation order: %v, want %v", service.events, tc.wantEvents)
			}
		})
	}
}

func TestUnenrollPreservesNumericCloudV1Routing(t *testing.T) {
	for _, tc := range []struct {
		name       string
		org, asset int32
		legacy     bool
	}{
		{"legacy", 55, 277, true},
		{"minimum valid numeric IDs", 1, 1, true},
		{"UUID enrollment", 0, 0, false},
		{"missing organization", 0, 277, false},
		{"missing asset", 55, 0, false},
		{"invalid organization", -1, 277, false},
		{"invalid asset", 55, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isLegacyCloudEnrollment(tc.org, tc.asset); got != tc.legacy {
				t.Fatalf("legacy routing = %v, want %v", got, tc.legacy)
			}
		})
	}
}

type legacyUnenrollListPage struct {
	offset       int32
	certificates []*cloudpb.Certificate
	total        int32
	err          error
}

type legacyUnenrollSequenceServer struct {
	*legacyUnenrollServer
	pages       []legacyUnenrollListPage
	listCalls   int
	memberPages []legacyUnenrollMembersPage
	memberCalls int
}

type legacyUnenrollMembersPage struct {
	token     string
	members   []*cloudpb.OrganizationMember
	nextToken string
	err       error
}

func (s *legacyUnenrollSequenceServer) ListMembers(_ context.Context, req *cloudpb.ListMembersRequest) (*cloudpb.ListMembersResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	index := s.memberCalls
	s.memberCalls++
	s.events = append(s.events, fmt.Sprintf("members %q", req.GetPageToken()))
	if index >= len(s.memberPages) {
		return nil, status.Error(codes.Internal, "unexpected organization member list call")
	}
	page := s.memberPages[index]
	if req.GetOrganizationId() != 55 || req.GetPageToken() != page.token {
		return nil, status.Errorf(codes.InvalidArgument, "unexpected member query: organization=%d token=%q", req.GetOrganizationId(), req.GetPageToken())
	}
	if page.err != nil {
		return nil, page.err
	}
	return &cloudpb.ListMembersResponse{Members: page.members, NextPageToken: page.nextToken}, nil
}

func (s *legacyUnenrollSequenceServer) ListCertificates(req *cloudpb.ListCertificatesRequest, stream grpc.ServerStreamingServer[cloudpb.ListCertificatesResponse]) error {
	s.mu.Lock()
	index := s.listCalls
	s.listCalls++
	s.events = append(s.events, fmt.Sprintf("list %d", req.GetOffset()))
	s.mu.Unlock()
	if index >= len(s.pages) {
		return status.Error(codes.Internal, "unexpected certificate list call")
	}
	page := s.pages[index]
	if req.GetAssetId() != 42 || req.GetOffset() != page.offset || req.GetLimit() != 200 {
		return status.Errorf(codes.InvalidArgument, "unexpected certificate query: asset=%d offset=%d limit=%d", req.GetAssetId(), req.GetOffset(), req.GetLimit())
	}
	for _, certificate := range page.certificates {
		if err := stream.Send(&cloudpb.ListCertificatesResponse{Certificate: certificate, Total: page.total}); err != nil {
			return err
		}
	}
	return page.err
}

func startLegacyUnenrollSequenceServer(t *testing.T, pages []legacyUnenrollListPage) (*legacyUnenrollSequenceServer, *grpc.ClientConn) {
	t.Helper()
	service := &legacyUnenrollSequenceServer{
		legacyUnenrollServer: &legacyUnenrollServer{revokeSuccess: true, deleteSuccess: true},
		pages:                pages,
		memberPages: []legacyUnenrollMembersPage{{members: []*cloudpb.OrganizationMember{
			{OrganizationId: 55, UserId: "selected-user", Role: cloudpb.OrganizationRole_ORGANIZATION_ROLE_ADMIN},
		}}},
	}
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	cloudpb.RegisterCertificateServiceServer(server, service)
	cloudpb.RegisterAssetServiceServer(server, service)
	cloudpb.RegisterOrganizationServiceServer(server, service)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	t.Cleanup(func() { listener.Close() })
	conn, err := grpc.NewClient("passthrough:///legacy-unenroll-sequence", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return service, conn
}

func activeLegacyUnenrollCertificate(id int32) *cloudpb.Certificate {
	return &cloudpb.Certificate{Id: id, Status: cloudpb.CertificateStatus_CERTIFICATE_STATUS_ACTIVE}
}

func TestLegacyDeviceUnenrollRequiresAdministrativeMembershipBeforeReset(t *testing.T) {
	admin := &cloudpb.OrganizationMember{OrganizationId: 55, UserId: "selected-user", Role: cloudpb.OrganizationRole_ORGANIZATION_ROLE_ADMIN}
	otherAdmin := &cloudpb.OrganizationMember{OrganizationId: 55, UserId: "other-user", Role: cloudpb.OrganizationRole_ORGANIZATION_ROLE_ADMIN}
	type testCase struct {
		name            string
		pages           []legacyUnenrollMembersPage
		missingIdentity bool
		allowed         bool
		wantCode        codes.Code
		wantEvents      []string
	}
	cases := []testCase{
		{
			name:            "missing selected identity",
			missingIdentity: true,
		},
		{
			name:       "nonmember cannot list organization members",
			pages:      []legacyUnenrollMembersPage{{err: status.Error(codes.PermissionDenied, "not an organization member")}},
			wantCode:   codes.PermissionDenied,
			wantEvents: []string{`members ""`},
		},
		{
			name:       "membership lookup unavailable",
			pages:      []legacyUnenrollMembersPage{{err: status.Error(codes.Unavailable, "organization service unavailable")}},
			wantCode:   codes.Unavailable,
			wantEvents: []string{`members ""`},
		},
		{
			name:       "selected caller absent from roster",
			pages:      []legacyUnenrollMembersPage{{members: []*cloudpb.OrganizationMember{otherAdmin}}},
			wantEvents: []string{`members ""`},
		},
		{
			name:       "empty organization roster",
			pages:      []legacyUnenrollMembersPage{{}},
			wantEvents: []string{`members ""`},
		},
		{
			name: "admin of another organization",
			pages: []legacyUnenrollMembersPage{{members: []*cloudpb.OrganizationMember{
				{OrganizationId: 99, UserId: "selected-user", Role: cloudpb.OrganizationRole_ORGANIZATION_ROLE_ADMIN},
			}}},
			wantEvents: []string{`members ""`},
		},
		{
			name: "admin appears on later page",
			pages: []legacyUnenrollMembersPage{
				{members: []*cloudpb.OrganizationMember{otherAdmin}, nextToken: "next"},
				{token: "next", members: []*cloudpb.OrganizationMember{admin}},
			},
			allowed:    true,
			wantEvents: []string{`members ""`, `members "next"`},
		},
		{
			name: "later membership page unavailable",
			pages: []legacyUnenrollMembersPage{
				{members: []*cloudpb.OrganizationMember{otherAdmin}, nextToken: "next"},
				{token: "next", err: status.Error(codes.Unavailable, "later membership page unavailable")},
			},
			wantCode:   codes.Unavailable,
			wantEvents: []string{`members ""`, `members "next"`},
		},
		{
			name: "repeated membership page token",
			pages: []legacyUnenrollMembersPage{
				{members: []*cloudpb.OrganizationMember{otherAdmin}, nextToken: "next"},
				{token: "next", members: []*cloudpb.OrganizationMember{otherAdmin}, nextToken: "next"},
			},
			wantEvents: []string{`members ""`, `members "next"`},
		},
	}
	for _, role := range []struct {
		name    string
		role    cloudpb.OrganizationRole
		allowed bool
	}{
		{"owner", cloudpb.OrganizationRole_ORGANIZATION_ROLE_OWNER, true},
		{"admin", cloudpb.OrganizationRole_ORGANIZATION_ROLE_ADMIN, true},
		{"billing manager", cloudpb.OrganizationRole_ORGANIZATION_ROLE_BILLING_MANAGER, false},
		{"member", cloudpb.OrganizationRole_ORGANIZATION_ROLE_MEMBER, false},
		{"viewer", cloudpb.OrganizationRole_ORGANIZATION_ROLE_VIEWER, false},
		{"unspecified role", cloudpb.OrganizationRole_ORGANIZATION_ROLE_UNSPECIFIED, false},
		{"unknown role", cloudpb.OrganizationRole(99), false},
	} {
		cases = append(cases, testCase{
			name: role.name,
			pages: []legacyUnenrollMembersPage{{members: []*cloudpb.OrganizationMember{
				otherAdmin,
				{OrganizationId: 55, UserId: "selected-user", Role: role.role},
			}}},
			allowed:    role.allowed,
			wantEvents: []string{`members ""`},
		})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := activeLegacyUnenrollCertificate(11)
			service, conn := startLegacyUnenrollSequenceServer(t, []legacyUnenrollListPage{
				{certificates: []*cloudpb.Certificate{first}},
				{certificates: []*cloudpb.Certificate{first}},
			})
			service.memberPages = tc.pages
			userID := "selected-user"
			if tc.missingIdentity {
				userID = ""
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			got, err := performLegacyDeviceUnenroll(ctx, conn, 55, 42, userID, func() error {
				service.mu.Lock()
				service.events = append(service.events, "reset")
				service.mu.Unlock()
				return nil
			})
			want := legacyUnenrollResult{}
			wantEvents := tc.wantEvents
			if tc.allowed {
				want = legacyUnenrollResult{DeviceReset: true, CertificatesRevoked: 1, AssetDeleted: true}
				wantEvents = append(wantEvents, "list 0", "reset", "list 0", "revoke 11", "delete 42")
				if err != nil {
					t.Fatalf("administrative caller was rejected: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("unverified or unauthorized caller was allowed to unenroll")
				}
				for _, message := range []string{"device was not reset", "owner or admin"} {
					if !strings.Contains(err.Error(), message) {
						t.Errorf("preflight error must explain %q: %v", message, err)
					}
				}
			}
			if tc.wantCode != codes.OK && status.Code(err) != tc.wantCode {
				t.Fatalf("error code = %v, want %v: %v", status.Code(err), tc.wantCode, err)
			}
			if got != want {
				t.Fatalf("result = %+v, want %+v", got, want)
			}
			service.mu.Lock()
			defer service.mu.Unlock()
			if !reflect.DeepEqual(service.events, wantEvents) {
				t.Fatalf("operation order = %v, want %v", service.events, wantEvents)
			}
		})
	}
}

func TestLegacyDeviceUnenrollPreflightsBeforeReset(t *testing.T) {
	first := activeLegacyUnenrollCertificate(11)
	later := activeLegacyUnenrollCertificate(13)
	for _, tc := range []struct {
		name       string
		pages      []legacyUnenrollListPage
		resetErr   error
		deleteErr  error
		want       legacyUnenrollResult
		wantError  string
		wantEvents []string
	}{
		{
			name:       "empty unknown error preserves enrollment",
			pages:      []legacyUnenrollListPage{{err: status.Error(codes.Unknown, "")}},
			wantError:  "device was not reset",
			wantEvents: []string{"list 0"},
		},
		{
			name:       "partial stream failure preserves enrollment",
			pages:      []legacyUnenrollListPage{{certificates: []*cloudpb.Certificate{first}, err: status.Error(codes.Unavailable, "certificate stream interrupted")}},
			wantError:  "device was not reset",
			wantEvents: []string{"list 0"},
		},
		{
			name: "later preflight page failure preserves enrollment",
			pages: []legacyUnenrollListPage{
				{certificates: []*cloudpb.Certificate{first}, total: 2},
				{offset: 1, err: status.Error(codes.Unavailable, "later page interrupted")},
			},
			wantError:  "device was not reset",
			wantEvents: []string{"list 0", "list 1"},
		},
		{
			name:       "reset failure preserves cloud enrollment",
			pages:      []legacyUnenrollListPage{{certificates: []*cloudpb.Certificate{first}}},
			resetErr:   errors.New("reset refused"),
			wantError:  "reset refused",
			wantEvents: []string{"list 0", "reset"},
		},
		{
			name: "relist after reset includes newly issued certificate",
			pages: []legacyUnenrollListPage{
				{certificates: []*cloudpb.Certificate{first}},
				{certificates: []*cloudpb.Certificate{first, later}},
			},
			want:       legacyUnenrollResult{DeviceReset: true, CertificatesRevoked: 2, AssetDeleted: true},
			wantEvents: []string{"list 0", "reset", "list 0", "revoke 11", "revoke 13", "delete 42"},
		},
		{
			name: "agent restart retains existing cleanup behavior",
			pages: []legacyUnenrollListPage{
				{certificates: []*cloudpb.Certificate{first}},
				{certificates: []*cloudpb.Certificate{first}},
			},
			resetErr:   status.Error(codes.Unavailable, "agent restarting"),
			want:       legacyUnenrollResult{DeviceReset: true, CertificatesRevoked: 1, AssetDeleted: true},
			wantEvents: []string{"list 0", "reset", "list 0", "revoke 11", "delete 42"},
		},
		{
			name: "post reset listing failure reports partial result",
			pages: []legacyUnenrollListPage{
				{certificates: []*cloudpb.Certificate{first}},
				{err: status.Error(codes.Unknown, "")},
			},
			want:       legacyUnenrollResult{DeviceReset: true},
			wantError:  "listing certificates",
			wantEvents: []string{"list 0", "reset", "list 0"},
		},
		{
			name: "post reset permission change reports partial result",
			pages: []legacyUnenrollListPage{
				{certificates: []*cloudpb.Certificate{first}},
				{certificates: []*cloudpb.Certificate{first}},
			},
			deleteErr:  status.Error(codes.PermissionDenied, "asset deletion refused"),
			want:       legacyUnenrollResult{DeviceReset: true, CertificatesRevoked: 1},
			wantError:  "asset deletion refused",
			wantEvents: []string{"list 0", "reset", "list 0", "revoke 11", "delete 42"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, conn := startLegacyUnenrollSequenceServer(t, tc.pages)
			service.deleteErr = tc.deleteErr
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			got, err := performLegacyDeviceUnenroll(ctx, conn, 55, 42, "selected-user", func() error {
				service.mu.Lock()
				service.events = append(service.events, "reset")
				service.mu.Unlock()
				return tc.resetErr
			})
			if got != tc.want || (err == nil) != (tc.wantError == "") || err != nil && !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("got result=%+v error=%v, want result=%+v error containing %q", got, err, tc.want, tc.wantError)
			}
			if tc.resetErr != nil && status.Code(tc.resetErr) != codes.Unavailable && !errors.Is(err, tc.resetErr) {
				t.Fatalf("reset error was not preserved: got %v, want %v", err, tc.resetErr)
			}
			service.mu.Lock()
			defer service.mu.Unlock()
			wantEvents := append([]string{`members ""`}, tc.wantEvents...)
			if !reflect.DeepEqual(service.events, wantEvents) {
				t.Fatalf("operation order = %v, want %v", service.events, wantEvents)
			}
		})
	}
}

func TestLegacyCloudUnenrollInventoriesAllPagesBeforeMutation(t *testing.T) {
	first := activeLegacyUnenrollCertificate(11)
	second := activeLegacyUnenrollCertificate(13)
	for _, tc := range []struct {
		name        string
		pages       []legacyUnenrollListPage
		wantRevoked int
		wantDeleted bool
		wantError   bool
		wantEvents  []string
	}{
		{
			name: "all pages revoked before deletion",
			pages: []legacyUnenrollListPage{
				{certificates: []*cloudpb.Certificate{first}, total: 2},
				{offset: 1, certificates: []*cloudpb.Certificate{second}, total: 2},
			},
			wantRevoked: 2,
			wantDeleted: true,
			wantEvents:  []string{"list 0", "list 1", "revoke 11", "revoke 13", "delete 42"},
		},
		{
			name: "later page stream error prevents all revocation",
			pages: []legacyUnenrollListPage{
				{certificates: []*cloudpb.Certificate{first}, total: 2},
				{offset: 1, certificates: []*cloudpb.Certificate{second}, total: 2, err: status.Error(codes.Unavailable, "later page interrupted")},
			},
			wantError:  true,
			wantEvents: []string{"list 0", "list 1"},
		},
		{
			name: "incomplete inventory prevents all revocation",
			pages: []legacyUnenrollListPage{
				{certificates: []*cloudpb.Certificate{first}, total: 2},
				{offset: 1},
			},
			wantError:  true,
			wantEvents: []string{"list 0", "list 1"},
		},
		{
			name: "repeated certificate prevents all revocation",
			pages: []legacyUnenrollListPage{
				{certificates: []*cloudpb.Certificate{first}, total: 2},
				{offset: 1, certificates: []*cloudpb.Certificate{first}, total: 2},
			},
			wantError:  true,
			wantEvents: []string{"list 0", "list 1"},
		},
		{
			name: "certificate for another asset prevents all revocation",
			pages: []legacyUnenrollListPage{{certificates: []*cloudpb.Certificate{
				first,
				{Id: 15, AssetId: int32Ptr(99), Status: cloudpb.CertificateStatus_CERTIFICATE_STATUS_ACTIVE},
			}, total: 2}},
			wantError:  true,
			wantEvents: []string{"list 0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, conn := startLegacyUnenrollSequenceServer(t, tc.pages)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			revoked, deleted, err := cloudUnenrollCleanupClient(ctx, conn, 42)
			if revoked != tc.wantRevoked || deleted != tc.wantDeleted || (err != nil) != tc.wantError {
				t.Fatalf("got revoked=%d deleted=%v error=%v, want revoked=%d deleted=%v error=%v", revoked, deleted, err, tc.wantRevoked, tc.wantDeleted, tc.wantError)
			}
			service.mu.Lock()
			defer service.mu.Unlock()
			if !reflect.DeepEqual(service.events, tc.wantEvents) {
				t.Fatalf("operation order = %v, want %v", service.events, tc.wantEvents)
			}
		})
	}
}
