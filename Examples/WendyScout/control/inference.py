"""Text-only llama-server boundary. Reject backend fetch/path/adapter features."""
import json
TOOLS={'scout_probe','script_write','script_run','scout_report'}
def validate(body,model):
    allowed={'model','messages','tools','tool_choice','stream','stream_options','max_tokens','temperature','top_p','stop','seed','frequency_penalty','presence_penalty','parallel_tool_calls','store'}
    if not isinstance(body,dict) or set(body)-allowed or body.get('model')!=model:raise ValueError('Unsupported inference fields/model')
    messages=body.get('messages')
    if not isinstance(messages,list) or not 1<=len(messages)<=128:raise ValueError('Message limit')
    def tool_calls(calls):
        if not isinstance(calls,list) or len(calls)>16:raise ValueError('Tool-call limit')
        for call in calls:
            f=call.get('function',{})
            if set(call)-{'id','type','function'} or call.get('type')!='function' or set(f)-{'name','arguments'} or f.get('name') not in TOOLS or not isinstance(f.get('arguments'),str):raise ValueError('Unsupported tool call')
    for m in messages:
        if not isinstance(m,dict) or set(m)-{'role','content','tool_calls','tool_call_id','name'} or m.get('role') not in {'system','user','assistant','tool'}:raise ValueError('Unsupported message')
        content=m.get('content')
        if content is None and m['role']=='assistant':pass
        elif isinstance(content,str):pass
        elif isinstance(content,list):
            for part in content:
                if not isinstance(part,dict) or set(part)!={'type','text'} or part['type']!='text' or not isinstance(part['text'],str):raise ValueError('Only text content is permitted')
        else:raise ValueError('Only text content is permitted')
        if 'tool_calls' in m:tool_calls(m['tool_calls'])
    if 'tools' in body:
        if not isinstance(body['tools'],list) or len(body['tools'])>4:raise ValueError('Tool schema limit')
        for tool in body['tools']:
            f=tool.get('function',{})
            if set(tool)!={'type','function'} or tool['type']!='function' or set(f)-{'name','description','parameters','strict'} or f.get('name') not in TOOLS:raise ValueError('Unapproved tool schema')
            def check_schema(value):
                if isinstance(value,dict):
                    if any(k in value for k in ['$ref','$id','image_url','audio_url']):raise ValueError('Schema references/fetch features forbidden')
                    for v in value.values():check_schema(v)
                elif isinstance(value,list):
                    for v in value:check_schema(v)
            check_schema(f.get('parameters',{}))
    if 'tool_choice' in body:
        choice=body['tool_choice']
        if choice not in ['auto','none','required']:
            if not isinstance(choice,dict) or set(choice)!={'type','function'} or choice.get('type')!='function' or set(choice.get('function',{}))!={'name'} or choice['function']['name'] not in TOOLS:raise ValueError('Unsupported tool selection')
    if 'stream_options' in body and (not isinstance(body['stream_options'],dict) or set(body['stream_options'])-{'include_usage'}):raise ValueError('Unsupported stream options')
    result=dict(body);result['max_tokens']=min(max(int(body.get('max_tokens',1536)),1),1536)
    # A text containing a URL is inert tokenizer input. URL-bearing content
    # objects, JSON schemas, image/audio payloads and server overrides are denied.
    if len(json.dumps(result).encode())>1024*1024:raise ValueError('Inference request limit')
    return result
