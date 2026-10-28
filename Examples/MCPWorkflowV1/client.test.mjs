import test from "node:test";
import assert from "node:assert/strict";
import {decode, runVerifiedCycle, requireComplete, recoverObservation} from "./client.mjs";
const wrap = value => ({content:[{type:"text",text:JSON.stringify(value)}]});
const good = () => ({reported_state:"completed",job_path:"/owned/job",target:"pinned",workflow_sha256:"b".repeat(64),receipt_path:"/local/receipt",receipt_sha256:"a".repeat(64),observation:{step_check:"passed",expected_steps:3,zero_exit_steps:3,timed_out_steps:0},artifacts:{complete:true,verified_files:3,unchanged_files:0}});
test("batching uses approved dispatch twice and retains only decisive second result",async()=>{
  const calls=[];const result=await runVerifiedCycle(async(name,args)=>{calls.push([name,args]);const r=good();r.reused=calls.length===2;r.artifacts.unchanged_files=r.reused?3:0;return wrap(r)},"/local/workflow.json");
  assert.equal(calls.length,2);assert.equal(result.reused,true);assert.deepEqual(calls[0],calls[1]);
});
test("lost submit response, denied execution and missing evidence stop without fallback",async()=>{
  for(const response of [{isError:true,content:[]},wrap({job_path:"/owned/job",observation_error:"lost"}),wrap({...good(),artifact_error:"hash mismatch"})]) {
    let calls=0;await assert.rejects(runVerifiedCycle(async()=>{calls++;return response},"/local/workflow.json"));assert.equal(calls,1);
  }
  assert.throws(()=>decode({content:[]}));
  assert.throws(()=>requireComplete({...good(),observation:{step_check:"passed",expected_steps:3}}));
  let calls=0;await assert.rejects(runVerifiedCycle(async()=>{calls++;const r=good();r.reused=calls===2;r.artifacts.unchanged_files=3;if(calls===2)r.target="changed";return wrap(r)},"/local/workflow.json"));assert.equal(calls,2);
  assert.throws(()=>requireComplete({...good(),artifacts:{complete:true,verified_files:0}}));
});
const identity={job_path:"/owned/job",target:"pinned",spec_sha256:"c".repeat(64)};
test("read-only recovery handles unavailable transport, preserves identity and requires collection",async()=>{
  let time=0;let calls=0;const seen=[];
  const result=await recoverObservation(async(name,args)=>{seen.push([name,args]);calls++;if(calls===1)throw Object.assign(new Error("offline"),{code:"UNAVAILABLE"});return wrap({reported_state:calls===2?"running":"completed",target:"pinned",cursor:"same-job"})},identity,{budgetMs:5000,now:()=>time,sleep:async ms=>{time+=ms}});
  assert.equal(result.observation.reported_state,"completed");assert.equal(result.collection_required,true);assert.equal(result.transport_retries,1);
  assert.ok(seen.every(([name,args])=>name==="job_inspect"&&args.job_path===identity.job_path));assert.equal(seen[1][1].wait_seconds,0);assert.ok(seen[2][1].since);
  assert.throws(()=>requireComplete(result));
});
test("permission errors and changed targets never retry or reconnect",async()=>{
  for(const fn of [()=>{throw Object.assign(new Error("denied"),{code:"PERMISSION_DENIED"})},()=>wrap({reported_state:"completed",target:"other"})]) {
    let calls=0;await assert.rejects(recoverObservation(async()=>{calls++;return fn()},identity));assert.equal(calls,1);
  }
  await assert.rejects(recoverObservation(async()=>assert.fail(),{job_path:"/unknown"}));
});
test("transport recovery ceiling and deadline leave completion unknown",async()=>{
  let time=0,calls=0;await assert.rejects(recoverObservation(async()=>{calls++;throw Object.assign(new Error("offline"),{code:"UNAVAILABLE"})},identity,{budgetMs:5000,now:()=>time,sleep:async ms=>{time+=ms}}));assert.equal(calls,4);
  time=0;const result=await recoverObservation(async()=>wrap({reported_state:"running",target:"pinned"}),identity,{budgetMs:300,now:()=>time,sleep:async ms=>{time+=ms}});assert.equal(result.completion,"unknown");
});
