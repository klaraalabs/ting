package com.klaraalabs.ting.notifications

import com.klaraalabs.ting.installations.InstallationStatus

class TingNotification(val identifier: String, val title: String, val body: String, data: Map<String, String>) {
    val data: Map<String, String> = data.toMap()
}

sealed interface TingEvent {
    data class StatusChanged(val status: InstallationStatus) : TingEvent
    class NotificationReceived(val notification: TingNotification) : TingEvent
    class NotificationOpened(val notification: TingNotification) : TingEvent
}
