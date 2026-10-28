//go:build linux
package localmesh
import("context";"crypto/tls";"errors";"fmt";"net";"sync";"time";"github.com/wendylabsinc/wendy/go/internal/shared/meshid")
func nativeServerTLS(c *Credentials)(*tls.Config,error){
 if c==nil || c.Verify==nil || len(c.Certificate.Certificate)==0{return nil,errors.New("native listener requires enrolled credentials")}
 return &tls.Config{Certificates:[]tls.Certificate{c.Certificate},MinVersion:tls.VersionTLS13,MaxVersion:tls.VersionTLS13,ClientAuth:tls.RequireAnyClientCert,NextProtos:[]string{StreamALPN},SessionTicketsDisabled:true,VerifyConnection:func(s tls.ConnectionState)error{if s.NegotiatedProtocol!=StreamALPN{return errors.New("native stream ALPN required")};_,err:=lanAuthenticatedPeer(c,s);return err}},nil
}
func RunTLSListener(ctx context.Context,node *Node,listener net.Listener,cost uint16)error{return runNetTLSListener(ctx,node,listener,cost,nil,"")}
func runNetTLSListener(parent context.Context,node *Node,listener net.Listener,cost uint16,policy *ConnectionPolicy,group string)error{
 config,err:=nativeServerTLS(node.Credentials);if err!=nil{return err};ctx,cancel:=context.WithCancel(parent);var workers sync.WaitGroup;defer workers.Wait();defer cancel();stop:=context.AfterFunc(ctx,func(){_ = listener.Close()});defer stop()
 connections,handshakes:=make(chan struct{},32),make(chan struct{},4)
 for{raw,err:=listener.Accept();if err!=nil{if ctx.Err()!=nil{return nil};return fmt.Errorf("native TLS listener: %w",err)}
  select{case connections<-struct{}{}:default:_ = raw.Close();continue};select{case handshakes<-struct{}{}:default:<-connections;_ =raw.Close();continue}
  workers.Add(1);go func(){defer workers.Done();defer func(){<-connections}();defer raw.Close();var lease *ConnectionLease;defer func(){if lease!=nil{lease.Release()}}();stop:=context.AfterFunc(ctx,func(){_ =raw.Close()});defer stop()
   if policy!=nil{lease=policy.ReserveGroup(fmt.Sprintf("tls-incoming:%p",raw),meshid.Device{},false,group);if lease==nil{<-handshakes;return}}
   secure:=tls.Server(raw,config);setup,done:=context.WithTimeout(ctx,60*time.Second);err:=secure.HandshakeContext(setup);done();<-handshakes;if err!=nil{return};peer,err:=lanAuthenticatedPeer(node.Credentials,secure.ConnectionState());if err!=nil{return};linkCtx:=ctx;if lease!=nil{lease.Identify(peer);lease.Progress();linkCtx=WithConnectionLease(ctx,lease)};_ =node.AttachStream(linkCtx,peer,secure,cost)
  }()
 }
}
