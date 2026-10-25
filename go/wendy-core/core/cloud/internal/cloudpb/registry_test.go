package cloudpb

import (
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

func TestNotificationDescriptorsStayPrivate(t *testing.T) {
	const path = "wendycloud/v2/notifications.proto"
	if _, err := privateFiles.FindFileByPath(path); err != nil {
		t.Fatal(err)
	}
	if _, err := protoregistry.GlobalFiles.FindFileByPath(path); err != protoregistry.NotFound {
		t.Fatalf("application file registered globally: %v", err)
	}
	if _, err := privateTypes.FindMessageByName("wendycloud.v2.Notification"); err != nil {
		t.Fatal(err)
	}
	if _, err := protoregistry.GlobalTypes.FindMessageByName("wendycloud.v2.Notification"); err != protoregistry.NotFound {
		t.Fatalf("application message registered globally: %v", err)
	}
	methods := File_wendycloud_v2_notifications_proto.Services().ByName("NotificationService").Methods()
	if signed, ok := proto.GetExtension(methods.ByName("CreateNotification").Options(), E_SignedRequest).(bool); !ok || !signed {
		t.Fatal("lost imported extension")
	}
	for _, method := range []string{"GetNotification", "ListNotificationsV2", "CountNotifications", "DeleteNotifications", "SetNotificationReadStatus", "DeleteNotificationsThrough", "SetNotificationReadStatusThrough"} {
		if signed, _ := proto.GetExtension(methods.ByName(protoreflect.Name(method)).Options(), E_SignedRequest).(bool); signed {
			t.Fatalf("%s changed to a SignedRequest RPC", method)
		}
	}
}
