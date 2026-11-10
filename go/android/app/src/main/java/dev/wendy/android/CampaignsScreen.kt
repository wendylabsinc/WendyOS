package dev.wendy.android

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import org.json.JSONArray
import org.json.JSONObject

@Composable
fun CampaignsScreen(model: WendyModel) {
    val state = rememberRemote(model, "Campaigns", model.session)
    val clips = rememberRemote(model, "CampaignClips", model.session)
    var create by remember { mutableStateOf(false) }
    var editing by remember { mutableStateOf<String?>(null) }
    var episode by remember { mutableStateOf<String?>(null) }
    if (episode != null) { EpisodeScreen(model, episode!!) { episode = null }; return }
    if (create) { CampaignEditor(model) { create = false; state.refresh() }; return }
    LazyColumn(contentPadding = PaddingValues(20.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
        item { PageTitle("Campaigns", "Record device data when an event occurs"); RemoteStatus(state); Button(onClick = { create = true }) { Text("Create campaign") } }
        items(state.value.items()) { campaign ->
            val name = campaign.text("Name")
            SectionCard(name) {
                Text(campaign.text("Summary")); Text(campaign.text("State")); campaign.text("Problem").takeIf { it.isNotEmpty() }?.let { Text(it, color = MaterialTheme.colorScheme.error) }
                TextButton(onClick = { editing = name }) { Text("Edit settings") }
                clips.value.obj().list("Items").filter { it.text("Campaign") == name }.forEach { clip -> TextButton(onClick = { episode = clip.text("ID") }) { Text("${clip.text("Reason")} · ${clip.text("State")}") } }
            }
        }
        item { RemoteStatus(clips); Text(clips.value.obj().text("Warning")); TextButton(onClick = { state.refresh(); clips.refresh() }) { Text("Refresh") } }
    }
    editing?.let { name -> CampaignSettingsDialog(model, name) { editing = null; state.refresh() } }
}
@Composable
fun CampaignSettingsDialog(model: WendyModel, name: String, done: () -> Unit) {
    val state = rememberRemote(model, "CampaignSettings", model.session, name)
    if (state.loading || state.error != null) { AlertDialog(onDismissRequest = done, title = { Text("Campaign settings") }, text = { RemoteStatus(state) }, confirmButton = { TextButton(onClick = done) { Text("Close") } }); return }
    val data = state.value.obj()
    val fields = mutableListOf(Field("Before", "Before trigger", data.text("Before")), Field("After", "After trigger", data.text("After")), Field("Upload", "Upload policy", data.text("Upload")))
    if (data.flag("Inference")) fields.addAll(listOf(Field("Threshold", "Threshold, 0 to 1", data.text("Threshold"), numeric = true), Field("Cooldown", "Cooldown", data.text("Cooldown"))))
    InputDialog("Edit $name", fields, done, "Save") { values -> model.task {
        val updated = JSONObject(data.toString())
        values.forEach { (key, value) -> updated.put(key, if (key == "Threshold") value.toDouble() else value) }
        model.backend.call("UpdateCampaignSettings", model.session, updated); done()
    } }
}
@Composable
fun CampaignEditor(model: WendyModel, done: () -> Unit) {
    val sources = rememberRemote(model, "CampaignSources", model.session)
    var name by remember { mutableStateOf("") }
    var event by remember { mutableStateOf("") }
    var camera by remember { mutableStateOf("") }
    var telemetry by remember { mutableStateOf(true) }
    var upload by remember { mutableStateOf(false) }
    var before by remember { mutableStateOf("5") }
    var after by remember { mutableStateOf("10") }
    var inference by remember { mutableStateOf(false) }
    var repo by remember { mutableStateOf("") }
    var revision by remember { mutableStateOf("") }
    var labels by remember { mutableStateOf("person") }
    var threshold by remember { mutableStateOf("0.5") }
    val selected = remember { mutableStateMapOf<String,Boolean>() }
    Column(Modifier.verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        TextButton(onClick = done) { Text("Back to campaigns") }; PageTitle("Create campaign"); RemoteStatus(sources)
        OutlinedTextField(name, { name = it }, label = { Text("Name") })
        OutlinedTextField(event, { event = it }, label = { Text("Trigger event") })
        sources.value.items().forEach { source ->
            val id = source.text("id"); val kind = source.text("kind")
            Row { Text(source.text("name", "id"), Modifier.weight(1f)); Checkbox(checked = if (kind == "camera" || kind == "video") camera == id else selected[id] == true, onCheckedChange = { checked -> if (kind == "camera" || kind == "video") camera = if (checked) id else "" else selected[id] = checked }) }
        }
        Row { Text("Include telemetry", Modifier.weight(1f)); Switch(telemetry, { telemetry = it }) }
        Row { Text("Upload recordings", Modifier.weight(1f)); Switch(upload, { upload = it }) }
        Row { Text("Run inference", Modifier.weight(1f)); Switch(inference, { inference = it }) }
        if (inference) {
            OutlinedTextField(repo, { repo = it }, label = { Text("Model repository") })
            OutlinedTextField(revision, { revision = it }, label = { Text("Model revision") })
            OutlinedTextField(labels, { labels = it }, label = { Text("Labels, separated by commas") })
            OutlinedTextField(threshold, { threshold = it }, label = { Text("Threshold, 0 to 1") })
        }
        OutlinedTextField(before, { before = it }, label = { Text("Seconds before trigger") })
        OutlinedTextField(after, { after = it }, label = { Text("Seconds after trigger") })
        Button(enabled = name.isNotBlank(), onClick = { model.task {
            val all = sources.value.items()
            val audio = all.filter { selected[it.text("id")] == true && it.text("kind") == "audio" }.map { it.text("id") }
            val ros = all.filter { selected[it.text("id")] == true && it.text("kind").startsWith("ros") }.map { it.text("id") }
            model.backend.call("DeployCampaign", model.session, json("Name" to name, "Camera" to camera, "Event" to event, "Before" to before.toInt(), "After" to after.toInt(), "Telemetry" to telemetry, "Upload" to upload, "RunInference" to inference, "Model" to repo, "ModelRevision" to revision, "Labels" to JSONArray(labels.split(',').map { it.trim() }.filter { it.isNotEmpty() }), "Threshold" to threshold.toDouble(), "Cooldown" to 10, "Audio" to JSONArray(audio), "ROS2" to JSONArray(ros)))
            model.record("Created campaign $name"); done()
        } }) { Text("Create campaign") }
    }
}

@Composable fun CampaignEventButton(model: WendyModel) {}
