package com.klaraalabs.ting.notifications

import android.content.Intent
import com.google.firebase.messaging.RemoteMessage

object FirebaseNotifications {
    /** Received while foregrounded. The host owns notification display. */
    fun received(message: RemoteMessage): TingNotification? {
        val id = message.messageId ?: return null
        return TingNotification(id, message.notification?.title.orEmpty(), message.notification?.body.orEmpty(), message.data)
    }

    /** Call for initial Activity intent AND onNewIntent; background notification+data bypasses onMessageReceived. */
    fun opened(intent: Intent): TingNotification? {
        val id = intent.getStringExtra("google.message_id") ?: intent.getStringExtra("message_id") ?: return null
        val extras = intent.extras ?: return null
        val data = extras.keySet().filterNot {
            it.startsWith("google.") || it.startsWith("gcm.") || it in setOf("from", "collapse_key", "message_type", "message_id")
        }.mapNotNull { key -> intent.getStringExtra(key)?.let { key to it } }.toMap()
        // The launcher intent does not promise to contain the visible notification title/body.
        return TingNotification(id, "", "", data)
    }
}
