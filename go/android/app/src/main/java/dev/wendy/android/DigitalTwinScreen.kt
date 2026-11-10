package dev.wendy.android

import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.ViewInAr
import androidx.compose.runtime.Composable

/** The iOS digital twin needs local 3D site documents, which Android does not support yet. */
@Composable
fun DigitalTwinScreen(model: WendyModel) {
    PageColumn {
        item {
            EmptyMessage("Digital twin is not on Android yet",
                "The 3D site model, robot localization and navigation for ${model.selected?.title ?: "this device"} are available in the Wendy app for iPhone and iPad.",
                Icons.Outlined.ViewInAr)
        }
    }
}
