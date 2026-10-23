package com.lecywza.mytun;

import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Intent;
import android.os.Build;
import android.os.IBinder;
import android.os.PowerManager;
import android.util.Log;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.BufferedReader;
import java.io.File;
import java.io.IOException;
import java.io.InputStreamReader;
import java.net.HttpURLConnection;
import java.net.URL;
import java.util.ArrayList;
import java.util.List;
import java.util.concurrent.TimeUnit;

/**
 * Runs the bundled MyTun core as a child process inside a foreground service.
 *
 * Beyond starting the binary it does two things that matter on a phone:
 *   - holds a partial wake lock so Doze does not park the CPU and stall the
 *     tunnel while the screen is off;
 *   - polls the core's loopback status endpoint and mirrors the live state into
 *     the persistent notification, so the user can see whether the tunnel is
 *     still up after leaving the app.
 */
public class TunnelService extends Service {
    private static final String TAG = "MyTunSvc";
    private static final String CHANNEL_ID = "mytun-core";
    private static final int NOTIF_ID = 4101;
    public static final int PORT = 8899;
    private static final long STATUS_INTERVAL_MS = 5000;

    private Process process;
    private Thread statusThread;
    private volatile boolean stopping = false;
    private PowerManager.WakeLock wakeLock;
    private volatile String lastStatus = "";

    @Override
    public void onCreate() {
        super.onCreate();
        createChannel();
        acquireWakeLock();
    }

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        startForeground(NOTIF_ID, buildNotification("正在启动核心…", false, 0));
        if (process == null || !process.isAlive()) {
            new Thread(this::startCore, "mytun-start").start();
        }
        return START_STICKY;
    }

    private void acquireWakeLock() {
        if (wakeLock != null) return;
        try {
            PowerManager pm = (PowerManager) getSystemService(POWER_SERVICE);
            if (pm == null) return;
            wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "MyTun::core");
            wakeLock.setReferenceCounted(false);
            wakeLock.acquire();
        } catch (Throwable t) {
            Log.w(TAG, "wake lock failed: " + t);
        }
    }

    private void releaseWakeLock() {
        try {
            if (wakeLock != null && wakeLock.isHeld()) wakeLock.release();
        } catch (Throwable ignored) {
        }
        wakeLock = null;
    }

    private void startCore() {
        try {
            // Android only allows exec on native libs, so the core is packaged
            // as libmytuncore.so in jniLibs and executed from nativeLibraryDir.
            File bin = new File(getApplicationInfo().nativeLibraryDir, "libmytuncore.so");
            if (!bin.exists()) {
                throw new IOException("core binary missing: " + bin.getAbsolutePath());
            }
            if (!bin.canExecute()) {
                //noinspection ResultOfMethodCallIgnored
                bin.setExecutable(true, false);
            }

            File dataDir = new File(getFilesDir(), "data");
            if (!dataDir.exists() && !dataDir.mkdirs()) {
                Log.w(TAG, "cannot create data dir");
            }

            List<String> cmd = new ArrayList<>();
            cmd.add(bin.getAbsolutePath());
            cmd.add("--data");
            cmd.add(dataDir.getAbsolutePath());
            cmd.add("--host");
            cmd.add("127.0.0.1");
            cmd.add("--port");
            cmd.add(String.valueOf(PORT));
            String dns = activeDNSServer();
            if (dns != null) {
                cmd.add("--dns");
                cmd.add(dns);
            }

            ProcessBuilder pb = new ProcessBuilder(cmd);
            pb.directory(getFilesDir());
            pb.redirectErrorStream(false);
            process = pb.start();
            pump(process.getInputStream(), "core-out");
            pump(process.getErrorStream(), "core-err");
            Log.i(TAG, "core started: " + process);
            startStatusPolling();
        } catch (Exception e) {
            Log.e(TAG, "failed to start core", e);
            updateNotification("核心启动失败: " + e.getMessage(), false, 0);
        }
    }

    /** Android has no resolv.conf, so hand the core the live network resolver. */
    private String activeDNSServer() {
        try {
            android.net.ConnectivityManager cm =
                    (android.net.ConnectivityManager) getSystemService(CONNECTIVITY_SERVICE);
            if (cm == null) return null;
            android.net.Network net = Build.VERSION.SDK_INT >= Build.VERSION_CODES.M
                    ? cm.getActiveNetwork() : null;
            if (net != null) {
                android.net.LinkProperties lp = cm.getLinkProperties(net);
                if (lp != null) {
                    for (java.net.InetAddress a : lp.getDnsServers()) {
                        if (a != null && !a.isLoopbackAddress()) {
                            return a.getHostAddress();
                        }
                    }
                }
            }
            Class<?> sp = Class.forName("android.os.SystemProperties");
            Object v = sp.getMethod("get", String.class, String.class).invoke(null, "net.dns1", "");
            String s = String.valueOf(v);
            return s.isEmpty() ? null : s;
        } catch (Throwable t) {
            Log.w(TAG, "dns detect failed: " + t);
            return null;
        }
    }

    /** Polls the loopback status endpoint and mirrors it into the notification. */
    private void startStatusPolling() {
        if (statusThread != null) return;
        statusThread = new Thread(() -> {
            while (!stopping) {
                try {
                    String body = httpGet("http://127.0.0.1:" + PORT + "/api/local/status");
                    JSONObject o = new JSONObject(body);
                    boolean online = o.optBoolean("online", false);
                    int count = o.optInt("count", 0);
                    int servers = o.optInt("servers", 0);
                    int port = o.optInt("port", 0);
                    String text;
                    if (online) {
                        JSONArray ids = o.optJSONArray("clients");
                        String who = ids != null && ids.length() > 0 ? ids.optString(0) : "";
                        text = "隧道在线 · " + count + " 个客户端" + (who.isEmpty() ? "" : "（" + who + "）");
                    } else if (servers > 0) {
                        text = "服务端监听 " + port + " · 等待客户端连接";
                    } else {
                        text = "服务未启动 · 点击打开控制台";
                    }
                    lastStatus = text;
                    updateNotification(text, online, count);
                } catch (Exception e) {
                    if (!stopping) {
                        lastStatus = "核心无响应，正在重试…";
                        updateNotification(lastStatus, false, 0);
                    }
                }
                try {
                    Thread.sleep(STATUS_INTERVAL_MS);
                } catch (InterruptedException e) {
                    return;
                }
            }
        }, "mytun-status");
        statusThread.setDaemon(true);
        statusThread.start();
    }

    private String httpGet(String urlStr) throws IOException {
        HttpURLConnection c = (HttpURLConnection) new URL(urlStr).openConnection();
        c.setConnectTimeout(3000);
        c.setReadTimeout(3000);
        try (BufferedReader r = new BufferedReader(new InputStreamReader(c.getInputStream()))) {
            StringBuilder sb = new StringBuilder();
            String line;
            while ((line = r.readLine()) != null) sb.append(line);
            return sb.toString();
        } finally {
            c.disconnect();
        }
    }

    private void pump(java.io.InputStream in, String tag) {
        Thread t = new Thread(() -> {
            try (BufferedReader r = new BufferedReader(new InputStreamReader(in))) {
                String line;
                while ((line = r.readLine()) != null) {
                    Log.d(TAG, tag + ": " + line);
                }
            } catch (IOException ignored) {
            }
        }, tag);
        t.setDaemon(true);
        t.start();
    }

    private void createChannel() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.O) return;
        NotificationManager nm = getSystemService(NotificationManager.class);
        if (nm == null) return;
        NotificationChannel ch = new NotificationChannel(CHANNEL_ID, getString(R.string.notif_channel),
                NotificationManager.IMPORTANCE_LOW);
        ch.setDescription("Shows the live tunnel state in the notification bar");
        ch.setShowBadge(false);
        nm.createNotificationChannel(ch);
    }

    private Notification buildNotification(String text, boolean online, int clients) {
        Notification.Builder b;
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            b = new Notification.Builder(this, CHANNEL_ID);
        } else {
            b = new Notification.Builder(this);
        }
        Intent open = new Intent(this, MainActivity.class);
        open.setFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP);
        int pendingFlags = Build.VERSION.SDK_INT >= Build.VERSION_CODES.M
                ? PendingIntent.FLAG_IMMUTABLE : 0;
        PendingIntent pi = PendingIntent.getActivity(this, 0, open, pendingFlags);
        int icon = online
                ? android.R.drawable.stat_sys_upload_done
                : android.R.drawable.stat_notify_sync;
        return b.setContentTitle("MyTun · " + (online ? "已连接" : "未连接"))
                .setContentText(text)
                .setSmallIcon(icon)
                .setOngoing(true)
                .setOnlyAlertOnce(true)
                .setShowWhen(false)
                .setContentIntent(pi)
                .build();
    }

    private void updateNotification(String text, boolean online, int clients) {
        try {
            NotificationManager nm = (NotificationManager) getSystemService(NOTIFICATION_SERVICE);
            if (nm != null) nm.notify(NOTIF_ID, buildNotification(text, online, clients));
        } catch (Throwable t) {
            Log.w(TAG, "notify failed: " + t);
        }
    }

    @Override
    public void onDestroy() {
        stopping = true;
        releaseWakeLock();
        if (process != null) {
            process.destroy();
            try {
                if (!process.waitFor(3, TimeUnit.SECONDS)) {
                    process.destroyForcibly();
                }
            } catch (InterruptedException ignored) {
                process.destroyForcibly();
            }
            process = null;
        }
        super.onDestroy();
    }

    /** Quick TCP probe used by the activity to know when the console is up. */
    static boolean portReady(int port, int timeoutMs) {
        try (java.net.Socket s = new java.net.Socket()) {
            s.connect(new java.net.InetSocketAddress("127.0.0.1", port), timeoutMs);
            return true;
        } catch (IOException e) {
            return false;
        }
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }
}
