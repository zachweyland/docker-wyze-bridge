#include <glib-unix.h>
#include <gst/gst.h>
#include <gst/rtsp-server/rtsp-server.h>

#include <errno.h>
#include <signal.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

typedef struct {
    char path[256];
    int video_port;
    int audio_port;
} stream_config;

typedef struct {
    GMainLoop *loop;
} signal_context;

static gboolean stop_main_loop(gpointer data) {
    signal_context *ctx = (signal_context *)data;
    if (ctx != NULL && ctx->loop != NULL) {
        g_main_loop_quit(ctx->loop);
    }
    return G_SOURCE_CONTINUE;
}

static gchar *build_launch_description(const stream_config *stream) {
    const gchar *video_caps =
        "application/x-rtp,media=video,encoding-name=H264,clock-rate=90000,payload=96";
    const gchar *audio_caps =
        "application/x-rtp,media=audio,encoding-name=PCMU,clock-rate=8000,channels=1,payload=0";

    if (stream->audio_port > 0) {
        return g_strdup_printf(
            "( "
            "udpsrc address=127.0.0.1 port=%d caps=\"%s\" "
            "! rtpjitterbuffer latency=200 "
            "! rtph264depay request-keyframe=true wait-for-keyframe=true "
            "! h264parse "
            "! rtph264pay name=pay0 pt=96 config-interval=1 mtu=1200 "
            "udpsrc address=127.0.0.1 port=%d caps=\"%s\" "
            "! rtpjitterbuffer latency=150 "
            "! rtppcmudepay "
            "! mulawdec "
            "! audioconvert "
            "! audioresample "
            "! audio/x-raw,rate=8000,channels=1 "
            "! mulawenc "
            "! rtppcmupay name=pay1 pt=0 "
            ")",
            stream->video_port,
            video_caps,
            stream->audio_port,
            audio_caps);
    }

    return g_strdup_printf(
        "( "
        "udpsrc address=127.0.0.1 port=%d caps=\"%s\" "
        "! rtpjitterbuffer latency=200 "
        "! rtph264depay request-keyframe=true wait-for-keyframe=true "
        "! h264parse "
        "! rtph264pay name=pay0 pt=96 config-interval=1 mtu=1200 "
        ")",
        stream->video_port,
        video_caps);
}

static int load_config(const char *config_path, GPtrArray *streams) {
    FILE *fp = fopen(config_path, "r");
    char line[512];

    if (fp == NULL) {
        fprintf(stderr, "[GST_RTSP] failed to open %s: %s\n", config_path, strerror(errno));
        return -1;
    }

    while (fgets(line, sizeof(line), fp) != NULL) {
        stream_config *stream = NULL;
        char path[256];
        int video_port = 0;
        int audio_port = 0;
        int fields = 0;

        if (line[0] == '#' || line[0] == '\n') {
            continue;
        }

        fields = sscanf(line, "%255s %d %d", path, &video_port, &audio_port);
        if (fields < 2) {
            continue;
        }

        stream = g_new0(stream_config, 1);
        g_strlcpy(stream->path, path, sizeof(stream->path));
        stream->video_port = video_port;
        stream->audio_port = fields >= 3 ? audio_port : 0;
        g_ptr_array_add(streams, stream);
    }

    fclose(fp);
    return 0;
}

int main(int argc, char *argv[]) {
    const char *config_path = NULL;
    const char *service = "8555";
    GOptionContext *context = NULL;
    GError *error = NULL;
    GOptionEntry entries[] = {
        {"config", 'c', 0, G_OPTION_ARG_FILENAME, &config_path, "Path to stream config", NULL},
        {"port", 'p', 0, G_OPTION_ARG_STRING, &service, "RTSP port", NULL},
        {NULL},
    };
    GPtrArray *streams = NULL;
    GstRTSPServer *server = NULL;
    GstRTSPMountPoints *mounts = NULL;
    GMainLoop *loop = NULL;
    signal_context signals = {0};

    context = g_option_context_new("- direct RTSP bridge for WHEP-fed streams");
    g_option_context_add_main_entries(context, entries, NULL);
    if (!g_option_context_parse(context, &argc, &argv, &error)) {
        fprintf(stderr, "[GST_RTSP] argument parse failed: %s\n", error->message);
        g_error_free(error);
        g_option_context_free(context);
        return 1;
    }
    g_option_context_free(context);

    if (config_path == NULL || config_path[0] == '\0') {
        fprintf(stderr, "[GST_RTSP] --config is required\n");
        return 1;
    }

    gst_init(&argc, &argv);

    streams = g_ptr_array_new_with_free_func(g_free);
    if (load_config(config_path, streams) != 0) {
        g_ptr_array_free(streams, TRUE);
        return 1;
    }

    server = gst_rtsp_server_new();
    mounts = gst_rtsp_server_get_mount_points(server);
    gst_rtsp_server_set_service(server, service);

    for (guint i = 0; i < streams->len; i++) {
        GstRTSPMediaFactory *factory = NULL;
        gchar *launch = NULL;
        gchar *mount_path = NULL;
        stream_config *stream = g_ptr_array_index(streams, i);

        if (stream == NULL || stream->video_port <= 0) {
            continue;
        }

        launch = build_launch_description(stream);
        mount_path = g_strdup_printf("/%s", stream->path);

        factory = gst_rtsp_media_factory_new();
        gst_rtsp_media_factory_set_shared(factory, TRUE);
        gst_rtsp_media_factory_set_launch(factory, launch);
        gst_rtsp_mount_points_add_factory(mounts, mount_path, factory);

        g_print(
            "[GST_RTSP] mounted rtsp://127.0.0.1:%s%s (video=%d audio=%d)\n",
            service,
            mount_path,
            stream->video_port,
            stream->audio_port);

        g_free(launch);
        g_free(mount_path);
    }

    g_object_unref(mounts);
    if (gst_rtsp_server_attach(server, NULL) == 0) {
        fprintf(stderr, "[GST_RTSP] failed to attach server on port %s\n", service);
        g_object_unref(server);
        g_ptr_array_free(streams, TRUE);
        return 1;
    }

    loop = g_main_loop_new(NULL, FALSE);
    signals.loop = loop;
    g_unix_signal_add(SIGINT, stop_main_loop, &signals);
    g_unix_signal_add(SIGTERM, stop_main_loop, &signals);

    g_print("[GST_RTSP] listening on rtsp://0.0.0.0:%s\n", service);
    g_main_loop_run(loop);

    g_main_loop_unref(loop);
    g_object_unref(server);
    g_ptr_array_free(streams, TRUE);
    return 0;
}
